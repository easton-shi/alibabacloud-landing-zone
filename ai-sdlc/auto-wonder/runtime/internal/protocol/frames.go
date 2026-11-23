// Package protocol defines the scheduler <-> executor wire frames.
//
// Field names mirror ai-sdlc/auto-wonder/src/main/java/com/aliyun/autowonder/
// websocket/frame/*.java. Optional fields keep omitempty so older servers and
// older runtimes can ignore unknown sections.
package protocol

// Frame is the minimum envelope shared by every frame.
type Frame struct {
	Type string `json:"type"`
}

// Dispatch mirrors TaskDispatchFrame.
type Dispatch struct {
	Type                       string                      `json:"type"`
	DispatchID                 int64                       `json:"dispatchId"`
	ExecutorID                 int64                       `json:"executorId"`
	TenantID                   int64                       `json:"tenantId"`
	WorkitemID                 int64                       `json:"workitemId"`
	IdempotencyKey             string                      `json:"idempotencyKey"`
	AgentID                    int64                       `json:"agentId"`
	AgentVersionID             int64                       `json:"agentVersionId"`
	SDLCStepID                 int64                       `json:"sdlcStepId"`
	Attempt                    int                         `json:"attempt"`
	DownloadURL                string                      `json:"downloadUrl"`
	MD5                        string                      `json:"md5"`
	Size                       int64                       `json:"size"`
	PackageID                  string                      `json:"packageId"`
	Checksum                   string                      `json:"checksum"`
	ChecksumAlgorithm          string                      `json:"checksumAlgorithm"`
	ChecksumScope              string                      `json:"checksumScope"`
	Issuer                     string                      `json:"issuer"`
	SignatureRef               string                      `json:"signatureRef"`
	Signature                  string                      `json:"signature"`
	SignatureAlgorithm         string                      `json:"signatureAlgorithm"`
	SignaturePublicKey         string                      `json:"signaturePublicKey"`
	ExpiresAt                  string                      `json:"expiresAt"`
	AllowCommit                *bool                       `json:"allowCommit"`
	AllowPush                  *bool                       `json:"allowPush"`
	AllowNetwork               *bool                       `json:"allowNetwork"`
	PackageRefreshPath         string                      `json:"packageRefreshPath"`
	ArtifactUploadPath         string                      `json:"artifactUploadPath"`
	ArtifactProtocolVersion    int                         `json:"artifactProtocolVersion"`
	CheckpointUploadPath       string                      `json:"checkpointUploadPath"`
	CheckpointRefreshPath      string                      `json:"checkpointRefreshPath"`
	MemorySnapshotPath         string                      `json:"memorySnapshotPath"`
	MemoryChangesPath          string                      `json:"memoryChangesPath"`
	MemoryMutationPath         string                      `json:"memoryMutationPath"`
	MemoryRecallPath           string                      `json:"memoryRecallPath"`
	MemoryStartingRevisions    map[string]int64            `json:"memoryStartingRevisions"`
	DebugLog                   *DebugLogDirective          `json:"debugLog"`
	ResumeMode                 string                      `json:"resumeMode"`
	ResumeSessionBehavior      string                      `json:"resumeSessionBehavior"`
	ResumeFromDispatchID       int64                       `json:"resumeFromDispatchId"`
	ResumeProvider             string                      `json:"resumeProvider"`
	ResumeSessionID            string                      `json:"resumeSessionId"`
	ResumeCheckpointURL        string                      `json:"resumeCheckpointUrl"`
	ResumeCheckpointSHA256     string                      `json:"resumeCheckpointSha256"`
	ResumeCheckpointSeq        int64                       `json:"resumeCheckpointSeq"`
	ResumeCheckpointCandidates []ResumeCheckpointCandidate `json:"resumeCheckpointCandidates,omitempty"`
	DispatchMCPToken           string                      `json:"dispatchMcpToken"`
	EnvironmentVariables       map[string]string           `json:"environmentVariables"`
	MCPSecrets                 map[string]string           `json:"mcpSecrets"`
	ModelSelection             *ModelSelection             `json:"modelSelection"`
}

// ResumeCheckpointCandidate is one fallback checkpoint URL offered by the server.
type ResumeCheckpointCandidate struct {
	DownloadURL   string `json:"downloadUrl"`
	SHA256        string `json:"sha256"`
	CheckpointSeq int64  `json:"checkpointSeq"`
}

// DebugLogDirective is sent only after the executor declares DEBUG_LOG_V1.
type DebugLogDirective struct {
	Enabled       bool   `json:"enabled"`
	RoleCode      string `json:"roleCode"`
	MaxBytes      int64  `json:"maxBytes"`
	UploadPath    string `json:"uploadPath"`
	RelayFallback bool   `json:"relayFallback"`
}

// ModelSelection mirrors the dispatch model selection payload.
type ModelSelection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Ack acknowledges a dispatch (DISPATCHED -> ACKED).
type Ack struct {
	Type       string `json:"type"`
	DispatchID int64  `json:"dispatchId"`
}

// PauseFailed tells the scheduler that the runtime stopped the Agent but could
// not produce the checkpoint required by TASK_PAUSED.
type PauseFailed struct {
	Type       string `json:"type"`
	DispatchID int64  `json:"dispatchId"`
	Error      string `json:"error"`
}

// TaskSteer is a terminal human steering request for an active dispatch turn.
// The runtime cooperatively cancels the active provider turn, waits for it to
// exit, and resumes the same agent session with Content.
type TaskSteer struct {
	Type       string `json:"type"`
	GuidanceID int64  `json:"guidanceId"`
	DispatchID int64  `json:"dispatchId"`
	ExecutorID int64  `json:"executorId,omitempty"`
	Content    string `json:"content"`
}

// TaskSteerAck acknowledges a terminal steering request. Status is APPLIED
// once the steering content has been handed to the provider session, or FAILED
// when no active turn exists or submission fails.
type TaskSteerAck struct {
	Type       string `json:"type"`
	GuidanceID int64  `json:"guidanceId"`
	DispatchID int64  `json:"dispatchId"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// TaskGuidance is a non-terminal guidance batch for a dispatch or conversation.
type TaskGuidance struct {
	Type          string `json:"type"`
	GuidanceID    int64  `json:"guidanceId"`
	DispatchID    int64  `json:"dispatchId"`
	WorkitemID    int64  `json:"workitemId,omitempty"`
	TargetAgentID int64  `json:"targetAgentId,omitempty"`
	Content       string `json:"content"`
	Mode          string `json:"mode,omitempty"`
}

// TaskGuidanceAck acknowledges a non-terminal guidance batch.
type TaskGuidanceAck struct {
	Type          string `json:"type"`
	GuidanceID    int64  `json:"guidanceId"`
	DispatchID    int64  `json:"dispatchId"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	ReplyMarkdown string `json:"replyMarkdown,omitempty"`
}

// Progress moves a dispatch to RUNNING and feeds the UI timeline.
type Progress struct {
	Type          string `json:"type"`
	DispatchID    int64  `json:"dispatchId"`
	ResultSummary string `json:"resultSummary,omitempty"`
	Log           string `json:"log,omitempty"`
	StepOrder     int    `json:"stepOrder,omitempty"`
	StepID        int64  `json:"stepId,omitempty"`
	StepName      string `json:"stepName,omitempty"`
	Message       string `json:"message,omitempty"`
}

// Result reports the terminal outcome. Servers that support V2 require both
// resultProtocolVersion=2 and a stable resultId for idempotent replay.
type Result struct {
	Type                  string          `json:"type"`
	DispatchID            int64           `json:"dispatchId"`
	ResultProtocolVersion int             `json:"resultProtocolVersion"`
	ResultID              string          `json:"resultId"`
	Success               bool            `json:"success"`
	ResultSummary         string          `json:"resultSummary"`
	Error                 string          `json:"error"`
	FailureCategory       string          `json:"failureCategory,omitempty"`
	Retryable             *bool           `json:"retryable,omitempty"`
	CheckpointSeq         int64           `json:"checkpointSeq,omitempty"`
	CheckpointSHA256      string          `json:"checkpointSha256,omitempty"`
	DebugLog              *DebugLogReport `json:"debugLog,omitempty"`
}

// DebugLogReport closes the DEBUG_LOG_V1 loop in TASK_RESULT.
type DebugLogReport struct {
	Status    string `json:"status"`
	Channel   string `json:"channel,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated"`
	Error     string `json:"error,omitempty"`
}

// Paused reports a durable checkpoint after a non-cancel pause request.
type Paused struct {
	Type             string `json:"type"`
	DispatchID       int64  `json:"dispatchId"`
	CheckpointSeq    int64  `json:"checkpointSeq"`
	CheckpointSHA256 string `json:"checkpointSha256"`
}

// RestartResult reports executor restart progress or failure.
type RestartResult struct {
	Type       string `json:"type"`
	ExecutorID int64  `json:"executorId"`
	RequestID  string `json:"requestId"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// Handoff proposes the next owner after this dispatch.
type Handoff struct {
	Type       string `json:"type"`
	DispatchID int64  `json:"dispatchId"`
	WorkitemID int64  `json:"workitemId"`
	To         string `json:"to"`
	ToType     string `json:"toType"`
	NextRole   string `json:"nextRole"`
	Reason     string `json:"reason"`
}

// Heartbeat refreshes presence. dispatch_inventory_v1 requires the inventory
// block, otherwise the server fails the dispatch as protocol-incompatible.
type Heartbeat struct {
	Type                         string   `json:"type"`
	ProtocolFeatures             []string `json:"protocolFeatures"`
	Version                      string   `json:"version,omitempty"`
	Model                        string   `json:"model,omitempty"`
	StartedAt                    string   `json:"startedAt,omitempty"`
	RestartRequestID             string   `json:"restartRequestId,omitempty"`
	MaxConcurrentDispatches      int      `json:"maxConcurrentDispatches"`
	DispatchInventoryReady       bool     `json:"dispatchInventoryReady"`
	RunningDispatchIDs           []int64  `json:"runningDispatchIds"`
	OwnedDispatchIDs             []int64  `json:"ownedDispatchIds"`
	RunningConversationTurnIDs   []int64  `json:"runningConversationTurnIds"`
	ComputingConversationTurnIDs []int64  `json:"computingConversationTurnIds"`
	DispatchInventoryError       string   `json:"dispatchInventoryError,omitempty"`
}

// InventorySnapshot is the runtime-side view used to build a heartbeat.
type InventorySnapshot struct {
	Capacity     int
	Running      []int64
	Owned        []int64
	Conversation []int64
	Error        string
}

// ConversationTurn mirrors WsConversationTransport.send. Capability packages
// are content-addressed by SHA-256 and are not covered by the dispatch
// Ed25519 envelope.
type ConversationTurn struct {
	Type                                string            `json:"type"`
	ExecutorID                          int64             `json:"executorId"`
	ConversationID                      int64             `json:"conversationId"`
	TurnID                              int64             `json:"turnId"`
	AgentID                             int64             `json:"agentId"`
	DispatchAttempt                     int               `json:"dispatchAttempt"`
	RequestID                           string            `json:"requestId"`
	Content                             string            `json:"content"`
	CLISessionRef                       string            `json:"cliSessionRef"`
	SystemPrompt                        string            `json:"systemPrompt"`
	AgentVersionID                      int64             `json:"agentVersionId"`
	CapabilityDownloadURL               string            `json:"capabilityDownloadUrl"`
	CapabilitySHA256                    string            `json:"capabilitySha256"`
	CapabilityHash                      string            `json:"capabilityHash"`
	MCPToken                            string            `json:"mcpToken"`
	MCPSecrets                          map[string]string `json:"mcpSecrets"`
	EnvironmentVariables                map[string]string `json:"environmentVariables"`
	ConversationDeliveryProtocolVersion int               `json:"conversationDeliveryProtocolVersion"`
	ConversationAnswerProtocolVersion   int               `json:"conversationAnswerProtocolVersion"`
	ConversationActorProtocolVersion    int               `json:"conversationActorProtocolVersion"`
}

// ConversationCancel mirrors WsConversationTransport.sendCancel.
type ConversationCancel struct {
	Type            string `json:"type"`
	ExecutorID      int64  `json:"executorId"`
	ConversationID  int64  `json:"conversationId"`
	TurnID          int64  `json:"turnId"`
	DispatchAttempt int    `json:"dispatchAttempt"`
}

// ConversationElicitation is an ACP answer card opened by the runtime.
type ConversationElicitation struct {
	Type                  string         `json:"type"`
	ExecutorID            int64          `json:"executorId"`
	ConversationID        int64          `json:"conversationId"`
	TurnID                int64          `json:"turnId"`
	RequestID             string         `json:"requestId"`
	Action                string         `json:"action"`
	Content               map[string]any `json:"content,omitempty"`
	AnswerProtocolVersion int            `json:"answerProtocolVersion,omitempty"`
	AnswerID              string         `json:"answerId,omitempty"`
	DispatchAttempt       int            `json:"dispatchAttempt,omitempty"`
	RemainingTTLMs        int64          `json:"remainingTtlMs,omitempty"`
	ReceiptOnly           bool           `json:"receiptOnly,omitempty"`
}

// ConversationCommandsProbe asks the runtime for its slash-command catalog.
type ConversationCommandsProbe struct {
	Type           string `json:"type"`
	ExecutorID     int64  `json:"executorId"`
	ConversationID int64  `json:"conversationId"`
}

// ConversationTurnEvent is one ordered chunk of the runtime's turn stream.
type ConversationTurnEvent struct {
	Type                                string `json:"type"`
	ExecutorID                          int64  `json:"executorId"`
	ConversationID                      int64  `json:"conversationId"`
	TurnID                              int64  `json:"turnId"`
	DispatchAttempt                     int    `json:"dispatchAttempt,omitempty"`
	EventSeq                            int64  `json:"eventSeq"`
	ChunkIndex                          int    `json:"chunkIndex"`
	ChunkCount                          int    `json:"chunkCount"`
	EventType                           string `json:"eventType"`
	PayloadFragment                     string `json:"payloadFragment"`
	ConversationDeliveryProtocolVersion int    `json:"conversationDeliveryProtocolVersion,omitempty"`
}

// ConversationTurnACK is the terminal outcome of one turn.
type ConversationTurnACK struct {
	Type                                string                 `json:"type"`
	ExecutorID                          int64                  `json:"executorId"`
	ConversationID                      int64                  `json:"conversationId"`
	TurnID                              int64                  `json:"turnId"`
	DispatchAttempt                     int                    `json:"dispatchAttempt,omitempty"`
	Status                              string                 `json:"status"`
	Error                               string                 `json:"error,omitempty"`
	ReplyMarkdown                       string                 `json:"replyMarkdown,omitempty"`
	SessionID                           string                 `json:"sessionId,omitempty"`
	Usage                               *ConversationTurnUsage `json:"usage,omitempty"`
	ConversationDeliveryProtocolVersion int                    `json:"conversationDeliveryProtocolVersion,omitempty"`
}

// ConversationDeliveryAck confirms one reliable outbound frame.
type ConversationDeliveryAck struct {
	Type                                string `json:"type"`
	ConversationDeliveryProtocolVersion int    `json:"conversationDeliveryProtocolVersion"`
	FrameType                           string `json:"frameType"`
	ConversationID                      int64  `json:"conversationId"`
	TurnID                              int64  `json:"turnId"`
	DispatchAttempt                     int    `json:"dispatchAttempt"`
	EventSeq                            int64  `json:"eventSeq"`
	ChunkIndex                          int    `json:"chunkIndex"`
}

// ConversationTurnUsage mirrors the server's optional per-turn usage block.
type ConversationTurnUsage struct {
	Provider         string   `json:"provider,omitempty"`
	Model            string   `json:"model,omitempty"`
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
	CacheReadTokens  int64    `json:"cacheReadTokens"`
	CacheWriteTokens int64    `json:"cacheWriteTokens"`
	ReasoningTokens  int64    `json:"reasoningTokens"`
	Credits          *float64 `json:"credits,omitempty"`
}

// ConversationElicitationReplyACK acknowledges a reliable answer delivery.
type ConversationElicitationReplyACK struct {
	Type                  string `json:"type"`
	ExecutorID            int64  `json:"executorId"`
	ConversationID        int64  `json:"conversationId"`
	TurnID                int64  `json:"turnId"`
	DispatchAttempt       int    `json:"dispatchAttempt"`
	RequestID             string `json:"requestId"`
	AnswerID              string `json:"answerId"`
	Status                string `json:"status"`
	AnswerProtocolVersion int    `json:"answerProtocolVersion,omitempty"`
}

// ConversationCommandsResult answers a slash-command probe.
type ConversationCommandsResult struct {
	Type           string `json:"type"`
	ExecutorID     int64  `json:"executorId"`
	ConversationID int64  `json:"conversationId"`
	Status         string `json:"status"`
	Commands       string `json:"commands,omitempty"`
	Error          string `json:"error,omitempty"`
}
