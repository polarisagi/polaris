package hook

import (
	"encoding/json"
	"strings"
)

// isValidElicitAction 三态答复的合法取值（elicitation §Response Actions）；不用包级
// map 是为了不引入可变全局变量（CLAUDE.md §不变量 HE-6 相邻约束：internal/ 禁全局可变变量）。
func isValidElicitAction(a string) bool {
	switch a {
	case "accept", "decline", "cancel":
		return true
	default:
		return false
	}
}

// Input 标准 hook 输入（stdin JSON / http 请求体）。字段名与两家一致；事件特有字段按需填充。
type Input struct {
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id,omitempty"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  Event  `json:"hook_event_name"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode"`

	// 工具事件
	ToolName     string         `json:"tool_name,omitempty"`
	ToolInput    map[string]any `json:"tool_input,omitempty"`
	ToolUseID    string         `json:"tool_use_id,omitempty"`
	ToolResponse any            `json:"tool_response,omitempty"`
	Error        string         `json:"error,omitempty"`

	// UserPromptSubmit
	Prompt string `json:"prompt,omitempty"`
	// SessionStart: source（startup/resume/clear/compact）；SessionEnd: reason；Pre/PostCompact: trigger
	Source  string `json:"source,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Trigger string `json:"trigger,omitempty"`
	// Stop / SubagentStop：本轮已因 Stop hook 续跑过（防无限续跑，两家语义一致）
	StopHookActive       bool   `json:"stop_hook_active,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	// Subagent*
	AgentID   string `json:"agent_id,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	// Notification
	Message          string `json:"message,omitempty"`
	NotificationType string `json:"notification_type,omitempty"`
	// Elicitation*
	MCPServerName   string          `json:"mcp_server_name,omitempty"`
	Elicitation     map[string]any  `json:"elicitation,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitation_id,omitempty"`
	RequestedSchema json.RawMessage `json:"requested_schema,omitempty"`
	// Action / Content：Elicitation 事件为空（尚无答复）；ElicitationResult 事件为
	// 用户（或上游 Elicitor）已作出的答复，供 hook 观察/覆盖。
	Action  string         `json:"action,omitempty"`
	Content map[string]any `json:"content,omitempty"`
}

// MatchSubject 事件的 matcher 比对对象（两家约定）。
func (in Input) MatchSubject() string {
	switch in.HookEventName {
	case EventPreToolUse, EventPostToolUse, EventPostToolUseFailure, EventPermissionRequest:
		return in.ToolName
	case EventSessionStart:
		return in.Source
	case EventSessionEnd, EventStopFailure:
		return in.Reason
	case EventPreCompact, EventPostCompact:
		return in.Trigger
	case EventSubagentStart, EventSubagentStop:
		return in.AgentType
	case EventNotification:
		return in.NotificationType
	case EventElicitation, EventElicitationResult:
		return in.MCPServerName
	}
	return ""
}

// Decision 权限类决策。
type Decision string

const (
	DecisionNone  Decision = ""
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
)

// Outcome 一次事件分发的合并结果。
type Outcome struct {
	// Block 阻断当前动作（PreToolUse 拒绝工具、UserPromptSubmit 丢弃提示、Stop 要求继续等）。
	Block  bool
	Reason string
	// Decision PreToolUse / PermissionRequest 的权限决策（deny 优先于 ask 优先于 allow）。
	Decision Decision
	// UpdatedInput PreToolUse 改写后的工具入参（调用方须对改写结果重新过策略门）。
	UpdatedInput map[string]any
	// AdditionalContext 注入模型上下文的补充内容（TaintHigh）。
	AdditionalContext []string
	// Stop 要求终止整个回合（continue:false）。
	Stop           bool
	StopReason     string
	SystemMessages []string
	Errors         []string
	// ElicitAction / ElicitContent：Elicitation / ElicitationResult hook 的程序化答复
	// （accept/decline/cancel + 表单内容）。多个处理器命中时后到的覆盖先到的（两家一致）。
	ElicitAction  string
	ElicitContent map[string]any
}

// output 标准 JSON 输出（两家字段并集）。
type output struct {
	Continue           *bool           `json:"continue"`
	StopReason         string          `json:"stopReason"`
	SystemMessage      string          `json:"systemMessage"`
	SuppressOutput     bool            `json:"suppressOutput"`
	Decision           json.RawMessage `json:"decision"` // "block" 或 {"behavior": "allow|deny", "message": ...}
	Reason             string          `json:"reason"`
	HookSpecificOutput *specificOutput `json:"hookSpecificOutput"`
}

type specificOutput struct {
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason"`
	UpdatedInput             map[string]any  `json:"updatedInput"`
	AdditionalContext        string          `json:"additionalContext"`
	Decision                 json.RawMessage `json:"decision"`
	Continue                 *bool           `json:"continue"`
	StopReason               string          `json:"stopReason"`
	SystemMessage            string          `json:"systemMessage"`
	// Action / Content：Elicitation hook 的程序化答复，ElicitationResult hook 对
	// 用户答复的覆盖（claude_hooks_elicitation.md §Elicitation output / ElicitationResult output）。
	Action  string         `json:"action"`
	Content map[string]any `json:"content"`
}

// handlerResult 单个处理器的原始结果（与传输方式无关）。
type handlerResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Err      error
}

// merge 按两家语义把单个处理器结果并入 Outcome。
func (o *Outcome) merge(event Event, r handlerResult) {
	switch {
	case r.Err != nil:
		o.Errors = append(o.Errors, r.Err.Error())
	case r.ExitCode == 2:
		// 阻断：原因取 stderr；JSON 输出被忽略（两家一致）。
		o.block(event, firstNonBlank(string(r.Stderr), "blocked by hook"))
	case r.ExitCode != 0:
		o.Errors = append(o.Errors, firstNonBlank(string(r.Stderr), "hook exited non-zero"))
	default:
		o.mergeStdout(event, r.Stdout)
	}
}

func (o *Outcome) block(event Event, reason string) {
	o.Block = true
	o.Reason = joinReason(o.Reason, reason)
	if event == EventPreToolUse || event == EventPermissionRequest {
		o.Decision = DecisionDeny
	}
}

// mergeStdout 退出码 0：stdout 为 JSON 时解析决策；纯文本在 SessionStart / UserPromptSubmit /
// SubagentStart 作为附加上下文，其余事件忽略。
func (o *Outcome) mergeStdout(event Event, stdout []byte) {
	text := strings.TrimSpace(string(stdout))
	if text == "" {
		return
	}
	var out output
	if !strings.HasPrefix(text, "{") || json.Unmarshal([]byte(text), &out) != nil {
		if event == EventSessionStart || event == EventUserPromptSubmit || event == EventSubagentStart {
			o.AdditionalContext = append(o.AdditionalContext, text)
		}
		return
	}
	o.applyCommon(out.Continue, out.StopReason, out.SystemMessage)
	o.applyDecision(event, out.Decision, out.Reason)
	if s := out.HookSpecificOutput; s != nil {
		o.applyCommon(s.Continue, s.StopReason, s.SystemMessage)
		o.applyPermission(s.PermissionDecision, s.PermissionDecisionReason)
		o.applyDecision(event, s.Decision, s.PermissionDecisionReason)
		if s.UpdatedInput != nil && event == EventPreToolUse && o.Decision != DecisionDeny {
			o.UpdatedInput = s.UpdatedInput
		}
		if s.AdditionalContext != "" {
			o.AdditionalContext = append(o.AdditionalContext, s.AdditionalContext)
		}
		if event == EventElicitation || event == EventElicitationResult {
			o.applyElicitOutput(s)
		}
	}
}

// applyElicitOutput 合并 Elicitation / ElicitationResult hook 的 hookSpecificOutput。
// 非 accept/decline/cancel 的 action 值忽略（不覆盖已有决定）；content 独立覆盖，
// 使"只改 content 不改 action"这种覆盖也能表达。多个处理器命中时后到的覆盖先到的——
// mergeStdout 按 handlers 声明顺序被依次调用，这里的无条件赋值天然满足该顺序语义。
func (o *Outcome) applyElicitOutput(s *specificOutput) {
	if isValidElicitAction(s.Action) {
		o.ElicitAction = s.Action
	}
	if s.Content != nil {
		o.ElicitContent = s.Content
	}
}

func (o *Outcome) applyCommon(cont *bool, stopReason, systemMessage string) {
	if cont != nil && !*cont {
		o.Stop = true
		o.StopReason = joinReason(o.StopReason, stopReason)
	}
	if systemMessage != "" {
		o.SystemMessages = append(o.SystemMessages, systemMessage)
	}
}

// applyPermission deny > ask > allow（多个 hook 冲突时取最严格，两家一致）。
func (o *Outcome) applyPermission(decision, reason string) {
	d := Decision(strings.ToLower(decision))
	switch {
	case d == DecisionDeny:
		o.Decision, o.Block = DecisionDeny, true
		o.Reason = joinReason(o.Reason, reason)
	case d == DecisionAsk && o.Decision != DecisionDeny:
		o.Decision = DecisionAsk
		o.Reason = joinReason(o.Reason, reason)
	case d == DecisionAllow && o.Decision == DecisionNone:
		o.Decision = DecisionAllow
	}
}

// applyDecision 顶层 decision："block"（Stop/PostToolUse/UserPromptSubmit 等）或
// Codex PermissionRequest 的 {"behavior": "allow|deny", "message": "..."}。
func (o *Outcome) applyDecision(event Event, raw json.RawMessage, reason string) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.EqualFold(s, "block") {
			o.block(event, reason)
		}
		return
	}
	var obj struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Behavior != "" {
		o.applyPermission(obj.Behavior, obj.Message)
	}
}

func joinReason(a, b string) string {
	b = strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
