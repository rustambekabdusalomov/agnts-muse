package msp

import (
	"context"
	"fmt"
	"time"
)

// SchemaVersion is the MSP envelope version this client speaks.
const SchemaVersion = 1

// SchemaFingerprint is the stable-surface fingerprint of the schema these
// types were checked against (Muse Code 1.4.13). A server with another
// fingerprint may still be compatible; callers should log it, not fail.
const SchemaFingerprint = "sha256:7fddaf646dbc317e5fcac0785e135eb04df14fbb0db1863089dfd6da21730e5b"

// Methods (client → server).
const (
	MethodInitialize        = "initialize"
	MethodModelList         = "model/list"
	MethodSessionStart      = "session/start"
	MethodSessionResume     = "session/resume"
	MethodSessionSetModel   = "session/setModel"
	MethodSessionLeave      = "session/leave"
	MethodTurnStart         = "turn/start"
	MethodTurnSteer         = "turn/steer"
	MethodTurnInterrupt     = "turn/interrupt"
	MethodTurnCancel        = "turn/cancel"
	MethodApprovalDecide    = "approval/decide"
	MethodUsageRead         = "usage/read"
	NotificationInitialized = "initialized"
)

// Server requests (server → client).
const (
	RequestApproval  = "approval/request"
	RequestUserInput = "userInput/request"
)

// Notifications (server → client).
const (
	NoteTokenUsage         = "session/tokenUsage"
	NoteContextUsage       = "session/contextUsage"
	NoteSessionClosed      = "session/closed"
	NoteTurnStarted        = "turn/started"
	NoteTurnCompleted      = "turn/completed"
	NoteTurnRetry          = "turn/retryScheduled"
	NoteTurnModelRetry     = "turn/modelRetryScheduled"
	NoteItemStarted        = "item/started"
	NoteItemDelta          = "item/delta"
	NoteItemCompleted      = "item/completed"
	NoteApprovalRequested  = "approval/requested"
	NoteApprovalResolved   = "approval/resolved"
	NoteUsageChanged       = "usage/changed"
	NoteModelRouteUnserved = "session/modelRouteUnserved"
)

// ClientInfo identifies the client. Name must match ^[a-z0-9_]+$.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeParams opens a connection.
type InitializeParams struct {
	ClientInfo ClientInfo `json:"clientInfo"`
}

// SchemaInfo is the server's MSP schema.
type SchemaInfo struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

// ServerInfo names the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult describes the server.
type InitializeResult struct {
	ServerInfo          ServerInfo `json:"serverInfo"`
	UserAgent           string     `json:"userAgent"`
	Schema              SchemaInfo `json:"schema"`
	PlatformOS          string     `json:"platformOs"`
	SandboxAvailability string     `json:"sandboxAvailability,omitempty"`
	SessionDurability   string     `json:"sessionDurability,omitempty"`
}

// Initialize performs the handshake: initialize, then the initialized
// notification. It fails with ErrSchemaMismatch if the server speaks
// another envelope version.
func (c *Client) Initialize(ctx context.Context, info ClientInfo) (InitializeResult, error) {
	var res InitializeResult
	if err := c.Call(ctx, MethodInitialize, InitializeParams{ClientInfo: info}, &res); err != nil {
		return res, err
	}
	if res.Schema.Version != SchemaVersion {
		return res, fmt.Errorf("%w: server %d, client %d", ErrSchemaMismatch, res.Schema.Version, SchemaVersion)
	}
	return res, c.Notify(NotificationInitialized, struct{}{})
}

// ModelCost is a catalog price, as decimal strings per million tokens.
type ModelCost struct {
	Input    string  `json:"input"`
	Cached   string  `json:"cached"`
	Output   string  `json:"output"`
	Currency *string `json:"currency"`
}

// ModelEntry is one row of model/list.
type ModelEntry struct {
	ModelID                string     `json:"modelId"`
	DisplayLabel           string     `json:"displayLabel"`
	ProviderID             string     `json:"providerId"`
	ReleaseDate            string     `json:"releaseDate"`
	ContextLimit           int64      `json:"contextLimit"`
	OutputLimit            int64      `json:"outputLimit"`
	Cost                   *ModelCost `json:"cost"`
	IsDefault              bool       `json:"isDefault"`
	DefaultReasoningEffort string     `json:"defaultReasoningEffort,omitempty"`
}

// ModelListResult is the model catalog.
type ModelListResult struct {
	ProviderID string       `json:"providerId"`
	Source     string       `json:"source"`
	Models     []ModelEntry `json:"models"`
}

// Approval modes for SessionStartParams.ApprovalMode.
const (
	ApprovalAllowAll        = "allowAll"
	ApprovalPromptUnmatched = "promptUnmatched"
	ApprovalOnRequest       = "onRequest"
	ApprovalDenyUnmatched   = "denyUnmatched"
)

// SessionStartParams creates a session.
type SessionStartParams struct {
	CommandID         string `json:"commandId"`
	WorkspaceRoot     string `json:"workspaceRoot,omitempty"`
	ModelID           string `json:"modelId,omitempty"`
	ApprovalMode      string `json:"approvalMode,omitempty"`
	PermissionProfile string `json:"permissionProfile,omitempty"`
	SessionID         string `json:"sessionId,omitempty"`
}

// SessionResumeParams loads a stored session.
type SessionResumeParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
}

// Session is a session's state.
type Session struct {
	SessionID     string  `json:"sessionId"`
	Status        string  `json:"status"`
	ModelID       *string `json:"modelId"`
	ActiveTurnID  *string `json:"activeTurnId"`
	WorkspaceRoot string  `json:"workspaceRoot"`
	TurnCount     int     `json:"turnCount"`
}

// SessionResult is the result of session/start and session/resume.
type SessionResult struct {
	Session    Session `json:"session"`
	ViewCursor string  `json:"viewCursor"`
}

// ModelSelection names a model for session/setModel.
type ModelSelection struct {
	ModelID string `json:"modelId"`
}

// SessionSetModelParams changes a session's model for its next calls.
type SessionSetModelParams struct {
	CommandID string         `json:"commandId"`
	SessionID string         `json:"sessionId"`
	Model     ModelSelection `json:"model"`
}

// SessionLeaveParams releases a session.
type SessionLeaveParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	Stop      bool   `json:"stop,omitempty"`
}

// InputPart is one part of a turn's input.
type InputPart struct {
	Type       string `json:"type"` // "text" or "image"
	Text       string `json:"text,omitempty"`
	Base64Data string `json:"base64Data,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
}

// Text returns a text input part.
func Text(s string) InputPart { return InputPart{Type: "text", Text: s} }

// TurnStartParams submits input to a session.
type TurnStartParams struct {
	CommandID       string      `json:"commandId"`
	SessionID       string      `json:"sessionId"`
	Input           []InputPart `json:"input"`
	ReasoningEffort string      `json:"reasoningEffort,omitempty"`
	IfBusy          string      `json:"ifBusy,omitempty"`
}

// TurnStartResult says how the input was taken.
type TurnStartResult struct {
	TurnID         string `json:"turnId"`
	Disposition    string `json:"disposition"`
	StartedNewTurn bool   `json:"startedNewTurn"`
}

// TurnSteerParams injects input into the running turn.
type TurnSteerParams struct {
	CommandID      string      `json:"commandId"`
	SessionID      string      `json:"sessionId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []InputPart `json:"input"`
}

// TurnInterruptParams stops the running turn on the priority lane.
type TurnInterruptParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
}

// CommandAck is the result of a command (steer, interrupt, setModel, …).
type CommandAck struct {
	CommandID string `json:"commandId"`
	Status    string `json:"status"`
	TurnID    string `json:"turnId,omitempty"`
}

// RequirementRef identifies one stage of an approval.
type RequirementRef struct {
	ApprovalID  string `json:"approvalId"`
	SourceIndex int    `json:"sourceIndex"`
}

// ApprovalDecideParams decides a pending approval.
type ApprovalDecideParams struct {
	CommandID     string         `json:"commandId"`
	SessionID     string         `json:"sessionId"`
	ApprovalID    string         `json:"approvalId"`
	ChoiceID      string         `json:"choiceId"`
	RequirementID RequirementRef `json:"requirementId"`
	Feedback      *string        `json:"feedback,omitempty"`
}

// TokenCounts are the raw counters of one model call.
type TokenCounts struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CachedTokens    int64 `json:"cachedTokens"`
	ReasoningTokens int64 `json:"reasoningTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
}

// Cost is the server's cost estimate, when it has prices.
type Cost struct {
	USD     float64 `json:"usd"`
	Partial bool    `json:"partial"`
}

// CumulativeUsage is a session's running totals.
type CumulativeUsage struct {
	PromptTokens    int64 `json:"promptTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	TotalTokens     int64 `json:"totalTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	Cost            *Cost `json:"cost,omitempty"`
}

// TokenUsage is session/tokenUsage: one completed model call.
type TokenUsage struct {
	SessionID    string          `json:"sessionId"`
	TurnID       string          `json:"turnId"`
	ModelID      *string         `json:"modelId"`
	Usage        TokenCounts     `json:"usage"`
	Cumulative   CumulativeUsage `json:"cumulative"`
	FinishReason string          `json:"finishReason,omitempty"`
	DurationMs   int64           `json:"durationMs,omitempty"`
}

// Turn terminals.
const (
	TerminalCompleted = "completed"
	TerminalFailed    = "failed"
	TerminalCancelled = "cancelled"
)

// Quota is a provider limit that ended a turn.
type Quota struct {
	LimitKind  string `json:"limitKind"`
	ResetsAtMs int64  `json:"resetsAtMs"`
}

// ResetsAt is when the quota resets.
func (q Quota) ResetsAt() time.Time { return time.UnixMilli(q.ResetsAtMs) }

// TurnError is why a turn failed.
type TurnError struct {
	Kind      string `json:"kind"` // stepLimit, modelError, authRequired, …
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Quota     *Quota `json:"quota,omitempty"`
}

// TurnStarted is turn/started.
type TurnStarted struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
}

// TurnCompleted is turn/completed.
type TurnCompleted struct {
	SessionID  string       `json:"sessionId"`
	TurnID     string       `json:"turnId"`
	Terminal   string       `json:"terminal"`
	Reason     string       `json:"reason,omitempty"`
	Error      *TurnError   `json:"error,omitempty"`
	Usage      *TokenCounts `json:"usage,omitempty"`
	DurationMs int64        `json:"durationMs,omitempty"`
}

// Item kinds agnts-muse reads; others are passed through generically.
const (
	ItemUserMessage  = "userMessage"
	ItemAgentMessage = "agentMessage"
	ItemReasoning    = "reasoning"
	ItemToolCall     = "toolCall"
)

// Item statuses.
const (
	ItemInProgress = "inProgress"
	ItemCompleted  = "completed"
	ItemFailed     = "failed"
)

// Item is one transcript entry.
type Item struct {
	ItemID        string `json:"itemId"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	TurnID        string `json:"turnId,omitempty"`
	Text          string `json:"text,omitempty"`
	MessagePhase  string `json:"messagePhase,omitempty"` // "commentary" or "finalAnswer"
	Tool          string `json:"tool,omitempty"`
	CallID        string `json:"callId,omitempty"`
	Args          string `json:"args,omitempty"`
	VisibleOutput string `json:"visibleOutput,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	FallbackText  string `json:"fallbackText,omitempty"`
}

// ItemEvent is item/started and item/completed.
type ItemEvent struct {
	SessionID string `json:"sessionId"`
	Item      Item   `json:"item"`
}

// ItemDelta is item/delta: text appended to an open item's field.
type ItemDelta struct {
	SessionID string `json:"sessionId"`
	ItemID    string `json:"itemId"`
	Field     string `json:"field,omitempty"`
	Delta     string `json:"delta"`
}

// Approval decisions.
const (
	DecisionApproved = "approved"
	DecisionDenied   = "denied"
)

// ApprovalChoice is one option for deciding an approval.
type ApprovalChoice struct {
	ChoiceID        string `json:"choiceId"`
	Decision        string `json:"decision"`
	Label           string `json:"label"`
	Scope           string `json:"scope"`
	AcceptsFeedback bool   `json:"acceptsFeedback,omitempty"`
}

// ApprovalStage is one stage of a compound command.
type ApprovalStage struct {
	Argv []string `json:"argv,omitempty"`
}

// ApprovalSubject is what a tool call wants to do.
type ApprovalSubject struct {
	Kind          string          `json:"kind"` // shell, file, network, …
	Command       string          `json:"command,omitempty"`
	Path          string          `json:"path,omitempty"`
	Host          string          `json:"host,omitempty"`
	Target        string          `json:"target,omitempty"`
	Access        string          `json:"access,omitempty"`
	WorkspaceRoot string          `json:"workspaceRoot,omitempty"`
	Stages        []ApprovalStage `json:"stages,omitempty"`
}

// ApprovalRequested is approval/requested (and approval/request's params).
type ApprovalRequested struct {
	SessionID            string           `json:"sessionId"`
	ApprovalID           string           `json:"approvalId"`
	TurnID               string           `json:"turnId,omitempty"`
	ToolName             string           `json:"toolName"`
	RawArgs              string           `json:"rawArgs"`
	Subject              ApprovalSubject  `json:"subject"`
	AvailableChoices     []ApprovalChoice `json:"availableChoices"`
	CurrentRequirementID RequirementRef   `json:"currentRequirementId"`
	ProtectedWrite       bool             `json:"protectedWrite"`
}

// ApprovalResolved is approval/resolved.
type ApprovalResolved struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId"`
	Decision   string `json:"decision"`
	ResolvedBy string `json:"resolvedBy,omitempty"`
}

// UsageWindow is a subscription usage window.
type UsageWindow struct {
	UsedPercent        int   `json:"usedPercent"`
	ResetsAtMs         int64 `json:"resetsAtMs"`
	WindowDurationMins int   `json:"windowDurationMins,omitempty"`
}

// SubscriptionUsage is usage/changed and usage/read's usage.
type SubscriptionUsage struct {
	Window       UsageWindow `json:"window"`
	Weekly       UsageWindow `json:"weekly"`
	Tier         string      `json:"tier"`
	ObservedAtMs int64       `json:"observedAtMs"`
}

// UsageReadResult is usage/read's result.
type UsageReadResult struct {
	Usage *SubscriptionUsage `json:"usage,omitempty"`
}

// RetryScheduled is turn/retryScheduled and turn/modelRetryScheduled.
type RetryScheduled struct {
	SessionID    string `json:"sessionId"`
	TurnID       string `json:"turnId"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	RetryDelayMs int64  `json:"retryDelayMs"`
	HTTPStatus   int    `json:"httpStatus,omitempty"`
	ErrorKind    string `json:"errorKind,omitempty"`
	Reason       string `json:"reason,omitempty"`
}
