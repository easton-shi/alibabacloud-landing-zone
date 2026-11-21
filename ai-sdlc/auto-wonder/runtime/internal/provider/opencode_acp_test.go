package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeACP = `#!/usr/bin/env python3
import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

session = "ses_fake"

for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    ident = msg.get("id")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":ident,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":True}}})
    elif method == "session/load":
        send({"jsonrpc":"2.0","id":ident,"error":{"code":-32601,"message":"no such session"}})
    elif method == "session/new":
        send({"jsonrpc":"2.0","id":ident,"result":{"sessionId":session,"configOptions":[]}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"review","description":"review changes"}]}}})
    elif method == "session/set_config_option":
        send({"jsonrpc":"2.0","id":ident,"result":{"configOptions":[]}})
    elif method == "session/prompt":
        prompt_text = msg.get("params",{}).get("prompt",[{}])[0].get("text","")
        if "needs permission" in prompt_text:
            send({"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":session,"toolCall":{"toolCallId":"call-2","title":"Run shell","kind":"execute"},"options":[{"optionId":"once","name":"Allow once","kind":"allow_once"},{"optionId":"reject","name":"Reject","kind":"reject_once"}]}})
            for reply_line in sys.stdin:
                reply = json.loads(reply_line)
                if reply.get("id") == 99:
                    chosen = reply.get("result",{}).get("outcome",{}).get("optionId","")
                    send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"chosen:" + chosen}}}})
                    send({"jsonrpc":"2.0","id":ident,"result":{"stopReason":"end_turn"}})
                    break
            continue
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"tool_call","toolCallId":"call-1","name":"shell","title":"shell","kind":"execute","status":"pending","locations":[{"path":"/tmp"}],"rawInput":{"command":"echo hi"}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","title":"echo hi","kind":"execute","status":"in_progress","rawInput":{"command":"echo hi"}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","title":"echo hi","kind":"execute","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hi"}}],"rawOutput":{"exit":0}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}})
        send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"plan","entries":[{"content":"step","status":"completed"}]}}})
        send({"jsonrpc":"2.0","id":ident,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":2,"thoughtTokens":1,"cachedReadTokens":3}}})
`

func fakeACPExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte(fakeACP), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenCodeACPStreamsToolsPlansAndUsage(t *testing.T) {
	provider := &OpenCode{}
	var events []Event
	result, err := provider.Run(context.Background(), Request{
		AgentPath: fakeACPExecutable(t), WorkDir: t.TempDir(), Prompt: "run echo",
		UseACP: true, Model: "opencode/big-pickle",
	}, func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "ses_fake" || result.FullText != "done" {
		t.Fatalf("result = %#v", result)
	}
	if result.Usage == nil || result.Usage.InputTokens != 10 || result.Usage.CacheReadTokens != 3 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	var text, thinking, toolStart, toolResult, plan bool
	for _, event := range events {
		switch event.Kind {
		case "text":
			text = text || event.FullText == "done"
		case "thinking":
			thinking = thinking || event.FullText == "thinking"
		case "tool_start":
			toolStart = toolStart || event.ToolCallID == "call-1" && event.ToolKind == "execute"
		case "tool_update":
			toolResult = toolResult || event.ToolCallID == "call-1" && event.ToolStatus == "completed"
		case "plan":
			plan = plan || len(event.Plan) > 0
		}
	}
	if !text || !thinking || !toolStart || !toolResult || !plan {
		t.Fatalf("missing events: text=%t thinking=%t toolStart=%t toolResult=%t plan=%t events=%#v",
			text, thinking, toolStart, toolResult, plan, events)
	}
	if commands := provider.Commands(); len(commands) != 1 || commands[0]["name"] != "review" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestOpenCodeACPPermissionRoundTrip(t *testing.T) {
	provider := &OpenCode{}
	var text string
	_, err := provider.Run(context.Background(), Request{
		AgentPath: fakeACPExecutable(t), WorkDir: t.TempDir(), Prompt: "needs permission",
		UseACP: true,
		RequestInput: func(_ context.Context, input InputRequest) (InputAnswer, error) {
			if input.Kind != "APPROVAL" || !strings.Contains(input.Message, "Run shell") {
				t.Fatalf("unexpected permission request: %#v", input)
			}
			return InputAnswer{Action: "accept", Content: map[string]any{"optionId": "once"}}, nil
		},
	}, func(event Event) {
		if event.Kind == "text" {
			text += event.FullText
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "chosen:once" {
		t.Fatalf("text = %q", text)
	}
}
