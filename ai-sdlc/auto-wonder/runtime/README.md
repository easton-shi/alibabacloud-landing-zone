# AutoWonder Runtime (Community / Reverse-engineered)

这是一个面向 AutoWonder Community 的执行器 Runtime，基于官方公开的
`scheduler-executor-protocol` 与 Community 服务端代码实现，用来替代闭源的
`autowonder` npm Runtime / `auto-wonder-client-runtime`。

> **非官方组件。** 本项目不属于阿里云官方发布物，使用前请先在隔离环境验证。
> 仓库根目录的 MIT License 同样适用于本目录中的新增代码。

## 它做什么

Runtime 以 executor 身份连接 AutoWonder 服务端，完成一次 dispatch 的完整本地执行：

1. 连接 `/ws/executor`，周期发送带 inventory 的 `HEARTBEAT`。
2. 收到 `TASK_DISPATCH` 后回 `TASK_ACK`。
3. 下载任务包，校验 MD5、可选 SHA-256 与 Ed25519 签名，拒绝路径穿越和 ZIP 符号链接。
4. 读取 `repos.json`，把 eager 仓库检出到 `workspace/repos/<name>`；lazy 只读仓库列在提示词中，按需克隆。
5. 组装身份、当前 SDLC 步骤、工单、澄清、技能、MCP、仓库、记忆、队友和产物契约。
6. 合并任务包中的 MCP 配置与 dispatch token，写成 `0600` 的 `mcp.config.json`。
7. 拉起 Codex、OpenCode、Pi 或 Claude Code，把事件归一成 `TASK_PROGRESS`。
8. 记录最终 branch/HEAD 到 `deliverables/runtime-source-revision.json`。
9. 上传 `artifacts/output/**`，发送 `TASK_RESULT`，必要时发送 `TASK_HANDOFF`。
10. WebSocket 断开后指数退避重连；退出时取消在途 dispatch。
11. 收到非 cancel 的 `TASK_PAUSE` 时，把仓库提交状态打成 checkpoint 上传，并回 `TASK_PAUSED`；恢复时按 checkpoint 的 commit 重新检出。
12. 协商到 `DEBUG_LOG_V1` 时，把 dispatch 生命周期、Agent 事件和原始 stdout/stderr 写入有上限的日志，结束后直传或经产物中转上传。
13. 执行 `hooks.json` 中的生命周期 Hooks；`beforeCommit` / `beforePush` 通过仓库级 Git Hooks 执行。
14. 支持平台远程重启（空闲等待 / 强制）和同版本启动参数在线应用。
15. 处理 `CONVERSATION_TURN`：下载会话能力包、复用会话工作区和仓库、注入会话级 MCP token，
    以流式 `CONVERSATION_TURN_EVENT` 回传文本与状态，并用 `CONVERSATION_TURN_ACK` 收尾。
16. 支持 `CONVERSATION_TURN_CANCEL`、`CONVERSATION_COMPUTE_INVENTORY_V1` 心跳清单；
    Codex 额外支持 ACP 问答卡（`CONVERSATION_ACP_INTERACTION_V1`）。

## 支持的 Agent

| Provider | 调用方式 | 会话恢复 | 说明 |
| --- | --- | --- | --- |
| `codex` | `codex app-server --listen stdio://` + JSON-RPC | `thread/resume` | 通过 `-c mcp_servers.*` 注入 MCP，不修改用户全局 `~/.codex/config.toml` |
| `opencode` | `opencode run --format json` | `--session` | 默认适配公开版的 `--auto` + 位置参数；可切回闭源 daemon 的参数形状 |
| `pi` | `pi -p --mode json` | `--session <file>` | 会话文件落在 workspace 的 `.pi/sessions/` |
| `claude` | `claude -p --output-format stream-json` | `--resume` | 保留社区执行器的 headless 行为 |

Runtime 不使用统一的 ACP 协议驱动这四个 CLI。原因是它们的进程模型、权限参数、
流事件和会话恢复方式不同；差异收敛在 `internal/provider`，上层只处理统一的 `Event`。

交互式会话另有一条 ACP 桥：Codex app-server 的审批 / elicitation / requestUserInput
会转成 `acp_elicitation` 事件，用户回答后回填到原生 RPC。只有 Codex 声明
`CONVERSATION_ACP_INTERACTION_V1`；其他三个 provider 以文字提问。

## 环境要求

- Go 1.26 或更高版本（构建时）
- `git`、`ca-certificates`、宿主机可用的 SSH key / credential helper
- 至少一个 Agent CLI，并且已经完成登录
- 能访问 AutoWonder 的 WebSocket 和 HTTPS 端点

仓库认证不在任务包里传递明文凭据。Runtime 使用宿主机现有的 Git 配置，例如 SSH
agent、`~/.ssh/config`、Git credential helper 或企业 Git 客户端。

## 本地构建

```bash
cd ai-sdlc/auto-wonder/runtime
make build
./bin/autowonder-runtime
```

也可以只跑检查：

```bash
make test
make vet
```

## 配置

必需变量：

| 变量 | 示例 | 说明 |
| --- | --- | --- |
| `AUTOWONDER_SERVER_WS_URL` | `wss://autowonder.example.com` | 服务端地址；带不带 `/ws/executor` 都可以 |
| `AUTOWONDER_EXECUTOR_ID` | `10000` | 平台创建执行器时分配的 ID |
| `AUTOWONDER_EXECUTOR_TOKEN` | `aw...` | 执行器 token；只放环境变量或 `0600` env 文件 |

常用变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTOWONDER_PROVIDER` | `opencode` | `codex` / `opencode` / `pi` / `claude` |
| `AUTOWONDER_AGENT_PATH` | provider 默认命令 | Agent CLI 的绝对路径或 PATH 中的命令名 |
| `AUTOWONDER_AGENT_MODEL` | 空 | 默认模型；dispatch 的 `modelSelection` 优先 |
| `AUTOWONDER_REASONING_EFFORT` | 空 | 推理强度；Codex 映射为 `-c model_reasoning_effort=...` |
| `AUTOWONDER_CONTEXT_WINDOW` | 空 | 上下文窗口；Codex 映射为 `-c model_context_window=...` |
| `AUTOWONDER_WORKSPACE_ROOT` | `~/.autowonder/runtime` | 每次 dispatch 在这里建独立目录 |
| `AUTOWONDER_MAX_CONCURRENT_DISPATCHES` | `1` | 并发执行数，范围 1–50 |
| `AUTOWONDER_HEARTBEAT_SECONDS` | `30` | 心跳间隔，范围 5–60 秒 |
| `AUTOWONDER_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `AUTOWONDER_UNATTENDED` | `true` | Codex 是否附加无人值守参数 |
| `AUTOWONDER_UPLOAD_ARTIFACTS` | `true` | 是否上传 `artifacts/output/**` |
| `AUTOWONDER_REQUIRE_PACKAGE_SIGNATURE` | `false` | 为 `true` 时拒绝未签名的任务包；生产环境建议开启 |

Provider 专用变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTOWONDER_CODEX_PATH` | `codex` | 未设置 `AUTOWONDER_AGENT_PATH` 时使用 |
| `AUTOWONDER_OPENCODE_PATH` | `opencode` | 未设置 `AUTOWONDER_AGENT_PATH` 时使用 |
| `AUTOWONDER_PI_PATH` | `pi` | 未设置 `AUTOWONDER_AGENT_PATH` 时使用 |
| `AUTOWONDER_CLAUDE_PATH` | `claude` | 未设置 `AUTOWONDER_AGENT_PATH` 时使用 |
| `AUTOWONDER_OPENCODE_PERMISSION_FLAG` | `--auto` | 闭源 daemon 形状可设为 `--dangerously-skip-permissions` |
| `AUTOWONDER_OPENCODE_PROMPT_FLAG` | 空 | 闭源 daemon 形状可设为 `--prompt`；空表示使用位置参数 |
| `AUTOWONDER_PI_PROVIDER` | 空 | 传给 `pi --provider`；模型写成 `provider/model` 时也会自动拆分 |

## 快速启动

```bash
cd ai-sdlc/auto-wonder/runtime
make build

export AUTOWONDER_SERVER_WS_URL='wss://autowonder.example.com'
export AUTOWONDER_EXECUTOR_ID='10000'
export AUTOWONDER_EXECUTOR_TOKEN='replace-me'
export AUTOWONDER_PROVIDER='codex'
export AUTOWONDER_AGENT_PATH='/usr/local/bin/codex'
export AUTOWONDER_WORKSPACE_ROOT="$HOME/.autowonder/runtime"

./bin/autowonder-runtime
```

首次启动前确认 Agent 已经登录：

```bash
codex --version
codex login status
```

其他 provider 用各自官方方式安装并登录，然后设置对应的 `AUTOWONDER_AGENT_PATH`。

## Docker 部署

镜像只包含 Runtime、Git 和 SSH 客户端，不内置任何 Agent CLI。这样不会把模型凭据、
Node/Python 运行时和特定版本 CLI 固化进同一个镜像。

```bash
cd ai-sdlc/auto-wonder/runtime
docker build -t autowonder-runtime:local .
```

需要 Codex 时做一个很薄的派生镜像：

```dockerfile
FROM autowonder-runtime:local
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs npm \
    && npm install -g @openai/codex \
    && rm -rf /var/lib/apt/lists/*
USER autowonder
```

运行示例：

```bash
docker run --rm --name autowonder-runtime \
  --env-file "$HOME/.autowonder/runtime.env" \
  -e AUTOWONDER_PROVIDER=codex \
  -e AUTOWONDER_AGENT_PATH=/usr/local/bin/codex \
  -e AUTOWONDER_WORKSPACE_ROOT=/var/lib/autowonder/runtime \
  -v "$HOME/.autowonder/workspace:/var/lib/autowonder" \
  -v "$HOME/.codex:/home/autowonder/.codex" \
  -v "$HOME/.ssh:/home/autowonder/.ssh:ro" \
  -v "$HOME/.gitconfig:/home/autowonder/.gitconfig:ro" \
  autowonder-runtime:local
```

`runtime.env` 必须由你自行创建并设为 `0600`，不要把 token 写进镜像或仓库。

## 调度协议关键点

- `HEARTBEAT` 必须携带 `dispatchInventoryReady=true`、`maxConcurrentDispatches`、
  `runningDispatchIds` 和 `ownedDispatchIds`，否则新服务端会拒绝派发。
- `TASK_RESULT` 使用 V2 身份：`resultProtocolVersion=2` 加 UUID `resultId`。
- 服务端下发的 MCP 由两部分组成：`dispatchMcpToken` 对应的内置 `autowonder`
  server，以及任务包 `skills.json` 中声明的其他 MCP server。
- 任务包的 `repos.json` 是仓库权威来源；Runtime 不猜测仓库，也不会把 Git 凭据
  写入任务包或产物。
- 下一棒的 checkout 依据是 `artifacts/output/deliverables/runtime-source-revision.json`。
  Runtime 在 Agent 结束后写入本地 branch、base commit、最终 HEAD，并在能够向
  origin 验证时写入 `remoteHeadCommit`。
- `hooks.json` / `hook.yaml` 的 `beforeRepoPrepare`、`beforeStep`、`beforeAgentStart`、
  `afterAgentExit`、`beforeCommit`、`beforePush`、`cleanup` 由 Runtime 执行；
  Git 事件通过仓库级 `core.hooksPath` 安装，并链式保留仓库原有钩子。
- 执行器支持平台远程重启：`EXECUTOR_RESTART_V1`、`EXECUTOR_RESTART_POLICY_V1`
  （`WHEN_IDLE` / `FORCE`）和 `EXECUTOR_LAUNCH_COMMAND_V1`（同版本参数在线应用）。
- 平台 memory snapshot、changes、mutations、recalls 和 maintenance lease 已接入；
  Agent 仍优先通过内置 `autowonder` MCP 读写记忆，无法使用 MCP 时可通过
  `AUTOWONDER_LOCAL_MEMORY_ENDPOINT` 的 loopback 接口访问。

详细实现见 [docs/DESIGN.md](docs/DESIGN.md)。

## 已知边界

当前版本是一个可运行、可扩展的 Runtime，但不是闭源二进制的一比一还原：

- checkpoint 只保存已提交的仓库状态；Agent 未提交的工作区改动不会进入 checkpoint。
- 已实现 Ed25519 任务包签名校验；未签名任务包只在 `AUTOWONDER_REQUIRE_PACKAGE_SIGNATURE=false` 时放行。
- `beforeTool` / `afterTool` 尚未接入任何 provider 的原生工具拦截，因此不声明
  `TASK_PACKAGE_TOOL_HOOKS_V1`；绑定工具 hook 的任务会被服务端拒绝，而不是静默跳过。
- 人工门禁已桥接 Codex app-server 的审批、elicitation 和 requestUserInput；OpenCode、
  Pi、Claude 目前只能通过 `AUTOWONDER_LOCAL_API_ENDPOINT/request-input` 主动调用。
- 未声明 `EXECUTOR_UPDATE_V1` / `EXECUTOR_UPDATE_RESTART_V1`：Runtime 没有发布源，
  不能安全地下载并替换自身；跨版本升级需在宿主机完成，重启和同版本参数在线应用可用。
- 会话流式输出、取消和计算中清单已实现。ACP 问答卡只在 Codex 上声明
  `CONVERSATION_ACP_INTERACTION_V1`；OpenCode / Pi / Claude 没有原生交互 RPC，只能以文字提问。
- 会话的附件清单（`ATTACHMENT_MANIFEST_V1`）、在线产物回传（`ARTIFACT_OUTPUT_V1`）和
  行动计划（`ACTION_PLAN_V1`）尚未实现，因此不声明对应能力位。
- 未声明可靠投递（`CONVERSATION_DELIVERY_ACK_V1` / `CONVERSATION_ANSWER_DELIVERY_V1`）：
  WebSocket 断开时未确认的事件不会跨重连保留，依赖服务端的 stale turn 恢复重投。
- `allowCommit`、`allowPush` 当前通过提示词约束，不是 OS 级强制隔离；Git hook 只能
  阻止提交/推送动作，不能限制 Agent 直接改文件。
- `lazy` 只读仓库不会预克隆；提示词会给出地址、ref 和目标路径，由 Agent 判断需要时自行 clone。
- Debug log 已捕获原始 stdout/stderr，并对执行器 token、dispatch token、MCP secret
  和敏感环境变量做脱敏；脱敏基于已知值匹配，不能替代 Agent 自身避免输出密钥。
- OpenCode / Pi 的部分事件字段依赖版本；升级 CLI 后应先用真实输出校准解析器。

## 目录结构

```text
runtime/
├── cmd/autowonder-runtime/   # 进程入口、重连、心跳
├── internal/config/          # 环境变量
├── internal/protocol/        # WebSocket 帧
├── internal/ws/              # WebSocket 客户端
├── internal/capsule/         # 任务包下载与安全解压
├── internal/repos/           # 仓库物化与 source revision
├── internal/prompt/          # Agent 上下文拼装
├── internal/mcpconfig/       # MCP 配置合并
├── internal/provider/        # Codex / OpenCode / Pi / Claude
├── internal/hooks/           # Runtime Hooks 与 Git 前后置钩子
├── internal/memory/          # 平台记忆 snapshot / changes / mutations
├── internal/localapi/        # loopback request-input 与 memory 透传
├── internal/control/          # 远程重启与启动参数在线应用
├── internal/dispatch/        # 并发、生命周期、结果回传
├── internal/conversation/    # 交互式会话、流式事件、ACP 问答卡
└── internal/upload/          # artifacts 中转上传
```
