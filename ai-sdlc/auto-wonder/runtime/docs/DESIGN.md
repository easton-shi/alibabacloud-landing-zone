# AutoWonder Runtime 设计说明

## 1. 目标与边界

本目录实现的是 AutoWonder 服务端公开协议中的 **客户端执行器（executor）**：

- 通过 WebSocket 接收 dispatch。
- 在本地安全地准备任务包和仓库工作区。
- 使用本地 CLI Agent 完成一步 SDLC 工作。
- 回传进度、结果、交接和产物。

它不实现 AutoWonder 服务端、调度器、前端、数据库迁移或 OSS 服务。服务端仍是工单、
SDLC、派发、记忆和权限的权威来源。

选择 Go 的原因是与官方闭源 daemon 的运行时形态一致：单二进制、goroutine 处理
心跳和并发 dispatch、容易做进程信号和超时控制。

## 2. 进程模型

```text
autowonder-runtime process
├── WebSocket read loop
├── heartbeat goroutine
├── dispatch goroutine #1 ── child agent process
├── dispatch goroutine #2 ── child agent process
├── conversation turn #1 ── child agent process
├── conversation turn #2 ── child agent process
└── ...
```

每个 dispatch 有独立的：

- context 和 30 分钟执行超时；
- workspace 目录；
- provider 子进程；
- progress reporter；
- 结果 UUID。

同一时刻运行的 dispatch 数由 `AUTOWONDER_MAX_CONCURRENT_DISPATCHES` 控制。收到
`TASK_DISPATCH` 后先回 `TASK_ACK`，再等待信号量；等待期间该 dispatch 属于
`ownedDispatchIds`，真正执行时属于 `runningDispatchIds`。

会话轮次与 dispatch 是两套并发池：每轮 `CONVERSATION_TURN` 都会启动一个新的
provider 子进程，但共享该会话的 workspace 和 `cliSessionRef`。新对话意味着新的
conversation 目录和新的 CLI 会话；同一对话的连续轮次通过 provider 的 resume 参数
续接上下文。`runningConversationTurnIds` / `computingConversationTurnIds` 会随心跳上报。

WebSocket 断开时整个 session context 被取消，在途 Agent 子进程随之终止；主循环用
1s → 2s → ... → 30s 的指数退避重连。

## 3. 一次 dispatch 的生命周期

```text
TASK_DISPATCH
  │
  ├─ TASK_ACK
  ├─ download zip ── verify MD5 ── safe unzip
  ├─ repos.json ── git clone/fetch ── checkout pinned commit
  ├─ identity/sdlc/workitem/skills/repos/memory/roster -> prompt
  ├─ skills.json + dispatchMcpToken + mcpSecrets -> mcp.config.json
  ├─ provider subprocess
  │    ├─ Codex JSON-RPC
  │    ├─ OpenCode NDJSON
  │    ├─ Pi NDJSON
  │    └─ Claude stream-json
  ├─ git inspect -> runtime-source-revision.json
  ├─ upload artifacts/output/**
  ├─ TASK_HANDOFF (optional)
  └─ TASK_RESULT (V2)
```

失败也会尽力上传已经生成的产物和 source revision；上传失败只记 warn，不改变
dispatch 的结果，避免对象存储短暂故障掩盖真实的 Agent 执行结果。

## 4. 任务包与目录布局

服务端任务包结构由 `TaskPackager.java` 和
`docs/scheduler-executor-protocol.md` 定义。Runtime 解压后采用：

```text
<workspaceRoot>/dispatch-<id>/
├── package/                       # 任务包内容，只读参考
│   ├── identity.json
│   ├── sdlc.json
│   ├── workitem.md
│   ├── repos.json
│   ├── repo-map.json
│   ├── skills.json
│   ├── capabilities/skills/<name>/SKILL.md
│   ├── memory/*.md
│   └── roster.json
├── workspace/
│   └── repos/<repo-name>/         # 实际代码 checkout
├── artifacts/output/
│   ├── deliverables/
│   ├── evidence/
│   ├── patches/
│   ├── handoff/
│   └── learning_delta/
└── mcp.config.json                # 0600
```

解压只接受普通文件和目录；符号链接、绝对路径、`..` 路径穿越和 Windows 盘符路径
都会被拒绝。整包先按 `TASK_DISPATCH.md5` 校验。

## 5. Agent 如何知道该改哪个仓库

Runtime 不根据工单文本猜仓库，也不做全盘搜索。权威来源是任务包里的 `repos.json`：

```json
{
  "repos": [
    {
      "repoId": 1001,
      "name": "order-service",
      "url": "git@github.com:org/order-service.git",
      "ref": "a1b2c3...",
      "path": "order-service",
      "mode": "eager",
      "deliveryBranch": "aw/order-fix-20261120",
      "allowCommit": true,
      "allowPush": true,
      "allowNetwork": true
    }
  ]
}
```

这些条目来自服务端的员工版本仓库权限与当前轮次快照。`repo-map.json` 额外描述仓库
之间的关系，帮助 Agent 判断一个需求是否牵涉多个仓库。

会话轮次使用同一套格式：服务端把会话绑定 Agent 版本的仓库权限写进能力包的
`repos.json`，Runtime 物化到该会话的稳定 workspace。第二轮起复用已有 checkout，
避免把上一轮 Agent 的未提交改动重置掉。因此“改哪个仓库”始终由服务端下发的
`repos.json` 决定，而不是由模型猜测。

物化算法：

1. 校验仓库名和 `path` 都是单路径段，拒绝 `../x`、绝对路径和控制字符。
2. 目标不存在时执行 `git clone --no-checkout`。
3. 已存在时执行 `git fetch --prune --tags origin`。
4. 依次尝试 `pinnedBaseCommit`、`ref`、`deliveryBranch`、
   `sourceRevisionFallbackRefs`、`origin/HEAD`。
5. checkout 到解析出的 commit；如果有 `deliveryBranch`，创建或重置本地交付分支。
6. `mode=lazy` 的只读仓库不 checkout，只在提示词的 “Repositories available on demand”
   中列出名称、URL、ref 和建议路径，由 Agent 判断任务是否需要。
7. Agent 结束后读取最终 HEAD 和 branch，写入
   `deliverables/runtime-source-revision.json`。

Runtime 不保存 Git token。认证完全复用宿主机的 SSH agent、credential helper 和
Git 配置。这也意味着 Docker 部署必须显式挂载这些凭据。

平台管家会话会拿到租户的全部仓库，且全部是只读 `lazy`。如果预克隆，一次对话就会
把整个租户的代码拉下来；因此 Runtime 只物化 `eager` 仓库，`lazy` 仓库保留在提示词中
按需克隆。`repo-map.json` 同时给出仓库关系，帮助 Agent 判断一个需求牵涉哪些仓库。

## 6. 技能、插件和 MCP

`skills.json` 的 schema 是 `autowonder.capabilities.v1`：

- `skills[]`：技能元数据和 `path`，真正的技能正文在
  `capabilities/skills/<name>/SKILL.md`。
- `plugins[]`：插件元数据；是否安装由具体 Agent 决定。
- `mcpServers[]`：内置 `autowonder` MCP 和用户绑定的其他 MCP。

Runtime 不假设四家 Agent 有相同的技能目录。它把技能路径写进 prompt，要求 Agent
在遵守某个技能前先读取对应的 `SKILL.md`。这与官方“能力包随任务下发”的语义一致，
但把安装方式交给 Agent。

MCP 合并规则：

1. 有 `dispatchMcpToken` 时，内置 `autowonder` server 指向
   `<server>/api/mcp`，并带 `Authorization: Bearer <dispatch token>`。
2. 读取 `skills.json` 的其他 MCP server。
3. 用 `mcpSecrets` 解开 `headerSecretRefs` 和 `envSecretRefs`。
4. 输出 provider 中立配置，再由 Codex / OpenCode / Claude 适配。

内置 `autowonder` 的优先级最高，任务包不能用同名 MCP 覆盖它。配置文件权限固定为
`0600`。

## 7. Provider 适配

`internal/provider/provider.go` 定义三个概念：

```go
type Provider interface {
    Name() string
    Run(ctx context.Context, req Request, emit func(Event)) (Result, error)
}

type Event struct { Kind, Text, SessionID string }
type Result struct { Summary, SessionID string }
```

Provider 只负责“怎样启动 CLI、怎样解析 stdout、怎样恢复会话”，不接触 WebSocket、
任务包或 artifacts。

### Codex

- 启动 `codex app-server --listen stdio://`。
- JSON-RPC：`initialize` → `thread/start` 或 `thread/resume` → `turn/start`。
- 监听 `turn/completed` 结束一轮。
- 服务端请求：
  - approval → approved；
  - elicitation → accept 空内容；
  - requestUserInput → 空 answers。
- MCP 通过 `-c mcp_servers.<name>.*` 注入，不写用户全局配置。

### OpenCode

- 默认公开版形状：
  `opencode run --format json --auto --model ... --session ... <prompt>`。
- 闭源 daemon 形状可通过环境变量切回：
  `--dangerously-skip-permissions --prompt <prompt>`。
- 读取 `OPENCODE_CONFIG_CONTENT` 注入 MCP。

### Pi

- `pi -p --mode json [--session file] [--provider p] [--model m] <prompt>`。
- 新会话在 `.pi/sessions/` 建 jsonl 文件。
- Pi 的 stdout 可能混有 `call:` / `response:` 控制行，解析器会降级为文本/工具事件。

### Claude Code

- 保留社区执行器的 headless 参数。
- `--output-format stream-json --verbose`。
- 从 `assistant.message.content[]` 提取文本，从 `result` 提取最终摘要。

## 8. 协议帧

字段来源：

- `src/main/java/com/aliyun/autowonder/websocket/frame/TaskDispatchFrame.java`
- `src/main/java/com/aliyun/autowonder/websocket/InboundFrameRouter.java`
- `src/main/java/com/aliyun/autowonder/dispatch/DispatchResultCoordinator.java`
- `docs/scheduler-executor-protocol.md`

### `HEARTBEAT`

`dispatch_inventory_v1` 要求：

```json
{
  "type": "HEARTBEAT",
  "protocolFeatures": ["TASK_PACKAGE_SIGNATURE_V1", "dispatch_inventory_v1"],
  "maxConcurrentDispatches": 1,
  "dispatchInventoryReady": true,
  "runningDispatchIds": [],
  "ownedDispatchIds": [],
  "runningConversationTurnIds": [],
  "computingConversationTurnIds": []
}
```

### `CONVERSATION_TURN`

会话不是 dispatch：没有 SDLC 步骤、checkpoint、产物上传或 `TASK_RESULT`。每一轮由
服务端下发 `CONVERSATION_TURN`，Runtime 执行以下步骤：

1. 下载 `capabilityDownloadUrl` 并校验 `capabilitySha256`；能力包包含
   `skills.json`、`repos.json`、`repo-map.json` 和 `manifest.json`。
2. 仓库物化到 `<workspaceRoot>/conversations/<conversationId>/workspace/repos/`；
   后续轮次复用已存在的 checkout，不重置 Agent 未提交的改动。
3. 用本轮 `mcpToken` 和 `mcpSecrets` 生成 `0600` 的 `mcp.config.json`，内置
   `autowonder` MCP 指向服务端 `/api/mcp`。
4. 把 `systemPrompt`、技能清单、仓库清单和用户 `content` 组装成一轮 prompt，
   `cliSessionRef` 作为 provider 的恢复会话 ID。
5. 逐条上发 `CONVERSATION_TURN_EVENT`：

```json
{
  "type": "CONVERSATION_TURN_EVENT",
  "conversationId": 100,
  "turnId": 200,
  "dispatchAttempt": 1,
  "eventSeq": 3,
  "chunkIndex": 0,
  "chunkCount": 1,
  "eventType": "text",
  "payloadFragment": "{\"type\":\"text\",\"content\":\"hello\"}"
}
```

支持的 `eventType`：

| eventType | payload | 说明 |
| --- | --- | --- |
| `status` | `{"status":"RUNNING","sessionId":"..."}` | 轮次开始；`sessionId` 用于刷新 `cliSessionRef` |
| `text` | `{"content":"..."}` | 增量文本 |
| `thinking` | `{"content":"..."}` | 思考内容（provider 有则转发） |
| `log` | `{"content":"tool: ..."}` | 工具进度；provider 只给可读行，不伪造 tool_use/tool_result |
| `error` | `{"content":"..."}` | 引擎错误 |
| `acp_elicitation` | `{"data":{"requestId","mode","message","requestedSchema"}}` | Codex 原生人工输入卡 |
| `acp_elicitation_resolved` | `{"data":{"requestId","action","status","content"}}` | 卡片终态 |

6. 轮次结束发送 `CONVERSATION_TURN_ACK`：

```json
{
  "type": "CONVERSATION_TURN_ACK",
  "conversationId": 100,
  "turnId": 200,
  "dispatchAttempt": 1,
  "status": "SUCCESS",
  "replyMarkdown": "...",
  "sessionId": "..."
}
```

`status` 只使用服务端认可的 `SUCCESS` / `FAILED` / `CANCELED`。`CONVERSATION_TURN_CANCEL`
会取消对应轮次，并以已产出的部分文本作为 `CANCELED` 的 `replyMarkdown`。

`CONVERSATION_COMMANDS_PROBE` 当前回 `UNSUPPORTED`：四个 CLI 都没有稳定、可枚举的
斜杠命令接口，返回空列表会让前端误以为命令能力可用。

### `TASK_RESULT`

服务端 V2 校验要求 `resultProtocolVersion=2` 和非空 UUID `resultId`。缺少任一字段会
返回 `INVALID_RESULT_IDENTITY`。

### `TASK_HANDOFF`

Agent 写 `artifacts/output/handoff-proposal.json`，Runtime 读取后发送：

```json
{
  "type": "TASK_HANDOFF",
  "dispatchId": 1,
  "workitemId": 2,
  "to": "reviewer",
  "toType": "AGENT",
  "nextRole": "reviewer",
  "reason": "implementation complete"
}
```

## 9. 产物上传

上传接口：

```text
POST /api/daemon/dispatches/{dispatchId}/artifacts?token=<executorToken>
Content-Type: multipart/form-data
```

每个文件包含：

- `files`：文件内容；
- `filesMetadata`：`[{name,sizeBytes,sha256}]`。

Runtime 遍历 `artifacts/output/**`，把逻辑名写成
`artifacts/output/<relative-path>`。这和服务端 `DaemonArtifactController.logicalPath`
以及下一轮 `TaskPackager` 查找
`deliverables/runtime-source-revision.json` 的规则一致。

## 10. 安全模型

- 执行器 token 只从环境变量读取，不写日志、不写 workspace 文件。
- Dispatch MCP token 只写入 `0600` 的 `mcp.config.json`，随 dispatch 目录删除。
- 任务包解压做路径穿越和 symlink 防护。
- 仓库名和 path 必须是单路径段。
- Agent 运行在专属 OS 用户或容器中，不直接以 root 运行。
- Git 认证复用宿主机凭据；容器中应以只读方式挂载 SSH key 和 gitconfig。
- `allowCommit` / `allowPush` 目前是提示词约束，不是沙箱。真正需要强隔离时，应在
  容器网络、Git remote 权限或代理层限制 push。
- 任务包支持 SHA-256 与 Ed25519 校验；`AUTOWONDER_REQUIRE_PACKAGE_SIGNATURE=true`
  时拒绝未签名任务包。签名只证明包来自持有服务端私钥的一方，不替代 TLS 和 token 保护。
- Debug log 会记录 runtime 生命周期、Agent 归一化事件和原始 stdout/stderr；已知的
  执行器 token、dispatch token、MCP secret 和敏感环境变量会在写盘前替换为 `***`。
- Checkpoint 只保存已提交的仓库 commit；未提交的工作区改动不会随暂停恢复。

## 11. 扩展点

已补齐：

1. `DEBUG_LOG_V1`：gzip + sha256，直传失败时可走 artifacts 中转；捕获原始 stdout/stderr 并脱敏。
2. Checkpoint：`TASK_PAUSE` 时上传 tar.gz checkpoint，`TASK_PAUSED` 携带 seq/sha256；resume 时按 commit 恢复。
3. 任务包签名：使用 dispatch 中的 issuer/signatureRef/signature 校验 ZIP 归档。
4. Hooks：解析并校验 `hooks.json` / `hook.yaml`，执行 7 个生命周期事件；Git 事件通过
   `core.hooksPath` 安装并链式调用仓库原钩子。`beforeTool` / `afterTool` 未声明。
5. Memory：snapshot 注入、changes / mutations / recalls / maintenance lease 透传。
6. 人工门禁：Codex app-server 原生 RPC 已桥接；其他 provider 使用 loopback request-input。
7. 重启：`EXECUTOR_RESTART_V1`、`EXECUTOR_RESTART_POLICY_V1`、`EXECUTOR_LAUNCH_COMMAND_V1`。
8. 会话：`CONVERSATION_TURN_EVENT`、`CONVERSATION_TURN_CANCEL`、`CONVERSATION_COMPUTE_INVENTORY_V1`；
   Codex 额外实现 `CONVERSATION_ACP_INTERACTION_V1` 的问答卡闭环。

仍待补齐：

1. `beforeTool` / `afterTool` 的 provider 原生拦截（当前不声明 `TASK_PACKAGE_TOOL_HOOKS_V1`）。
2. OpenCode / Pi / Claude 原生审批桥接（当前只有 loopback request-input）。
3. `EXECUTOR_UPDATE_V1` / `EXECUTOR_UPDATE_RESTART_V1`：需要明确的发布源、校验和回滚机制。
4. 会话附件清单、在线产物回传和行动计划（`ATTACHMENT_MANIFEST_V1` / `ARTIFACT_OUTPUT_V1` / `ACTION_PLAN_V1`）。
5. OpenCode / Pi / Claude 的 ACP 交互；当前只有 Codex 声明 `CONVERSATION_ACP_INTERACTION_V1`。
6. 会话可靠投递：`CONVERSATION_DELIVERY_ACK_V1` / `CONVERSATION_ANSWER_DELIVERY_V1` 需要跨重连的 outbox；当前不声明。
7. 更细的 lazy 生命周期：例如会话内已克隆仓库的过期清理与并发去重。
