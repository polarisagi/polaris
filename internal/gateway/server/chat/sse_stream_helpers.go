package chat

// agentStreamRequest 是 HandleAgentStream 的请求体。
type agentStreamRequest struct {
	Input           string          `json:"input"`
	SessionID       string          `json:"session_id,omitempty"`
	RunID           string          `json:"run_id,omitempty"`
	ModelID         string          `json:"model_id,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Attachments     []sseAttachment `json:"attachments,omitempty"`
	// back-compat
	ImageParts []sseImagePart `json:"image_parts,omitempty"`
}

// buildStreamUserMessage / acquireStreamAgent / handleStreamFSMEvent
// [A-03 Step2/4] 已迁入 internal/gateway/session（分别为
// orchestrator.buildUserMessage / acquireInteractiveAgent / handleFSMEvent），
// 逻辑随 session.Orchestrator.RunTurn 统一收口，此处不再保留。
