package protocol

import (
	"encoding/json"
	"testing"
)

func TestResultV2WireShape(t *testing.T) {
	encoded, err := json.Marshal(Result{
		Type: "TASK_RESULT", DispatchID: 42, ResultProtocolVersion: 2,
		ResultID: "8a3f4e3e-1111-4222-8333-444455556666", Success: true, ResultSummary: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["resultProtocolVersion"] != float64(2) || decoded["resultId"] == "" {
		t.Fatalf("missing V2 identity: %s", encoded)
	}
}

func TestHeartbeatCarriesInventory(t *testing.T) {
	encoded, _ := json.Marshal(Heartbeat{
		Type: "HEARTBEAT", ProtocolFeatures: []string{"dispatch_inventory_v1"},
		MaxConcurrentDispatches: 2, DispatchInventoryReady: true,
		RunningDispatchIDs: []int64{7}, OwnedDispatchIDs: []int64{7, 8},
		RunningConversationTurnIDs: []int64{},
	})
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	if decoded["dispatchInventoryReady"] != true || decoded["maxConcurrentDispatches"] != float64(2) {
		t.Fatalf("heartbeat shape: %s", encoded)
	}
}

func TestDispatchCarriesArtifactProtocolVersion(t *testing.T) {
	encoded, err := json.Marshal(Dispatch{Type: "TASK_DISPATCH", DispatchID: 42, ArtifactProtocolVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["artifactProtocolVersion"] != float64(2) {
		t.Fatalf("artifact protocol missing: %s", encoded)
	}
}

func TestTerminalSteeringWireShape(t *testing.T) {
	encoded, err := json.Marshal(TaskSteer{
		Type: "TASK_STEER", GuidanceID: 77, DispatchID: 42, ExecutorID: 9, Content: "check concurrency",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"type": "TASK_STEER", "guidanceId": float64(77), "dispatchId": float64(42),
		"executorId": float64(9), "content": "check concurrency",
	} {
		if decoded[field] != want {
			t.Fatalf("%s = %v, want %v in %s", field, decoded[field], want, encoded)
		}
	}

	ack, err := json.Marshal(TaskSteerAck{
		Type: "TASK_STEER_ACK", GuidanceID: 77, DispatchID: 42, Status: "APPLIED",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ack, &decoded); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"type": "TASK_STEER_ACK", "guidanceId": float64(77), "dispatchId": float64(42), "status": "APPLIED",
	} {
		if decoded[field] != want {
			t.Fatalf("%s = %v, want %v in %s", field, decoded[field], want, ack)
		}
	}
}

func TestNonTerminalGuidanceWireShape(t *testing.T) {
	encoded, err := json.Marshal(TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 88, DispatchID: 42, WorkitemID: 7,
		TargetAgentID: 3, Content: "focus on tests", Mode: "SAFE_STEER",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"type": "TASK_GUIDANCE", "guidanceId": float64(88), "dispatchId": float64(42),
		"workitemId": float64(7), "targetAgentId": float64(3), "content": "focus on tests",
		"mode": "SAFE_STEER",
	} {
		if decoded[field] != want {
			t.Fatalf("%s = %v, want %v in %s", field, decoded[field], want, encoded)
		}
	}

	ack, err := json.Marshal(TaskGuidanceAck{
		Type: "TASK_GUIDANCE_ACK", GuidanceID: 88, DispatchID: 42, Status: "APPLIED",
		ReplyMarkdown: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ack, &decoded); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"type": "TASK_GUIDANCE_ACK", "guidanceId": float64(88), "dispatchId": float64(42),
		"status": "APPLIED", "replyMarkdown": "done",
	} {
		if decoded[field] != want {
			t.Fatalf("%s = %v, want %v in %s", field, decoded[field], want, ack)
		}
	}
}
