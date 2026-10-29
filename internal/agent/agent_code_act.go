package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runCodeAct 执行代码执行沙箱节点，staged 与 agentic 模式共用（R3-02, R4-03）。
func (a *Agent) runCodeAct(ctx context.Context, lang string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	if a.codeAct == nil {
		return nil, apperr.New(apperr.CodeInternal,
			"agent: codeAct engine not injected; cannot execute code_act node")
	}

	var codeArgs struct {
		Language        string           `json:"language"`
		Code            string           `json:"code"`
		CapabilityID    string           `json:"capability_id"`
		TaintLevel      types.TaintLevel `json:"taint_level"`
		SessionID       string           `json:"session_id"`
		StatefulSession bool             `json:"stateful_session"`
	}
	if err := json.Unmarshal(args, &codeArgs); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "code_act: unmarshal args", err)
	}

	if lang == "" {
		lang = codeArgs.Language
	}
	if lang == "" {
		lang = "python"
	}

	// JIT 铸造一次性 Capability Token，深度 0，容器沙箱 Sbx-L3
	jitTok, err := action.NewJITToken(a.ID,
		[]action.TokenOperation{{ToolName: lang, MaxCalls: 1}}, 3)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeForbidden, "code_act: JIT token mint failed", err)
	}

	targetSessionID, stateful := a.resolveCodeActSessionID(codeArgs.SessionID, codeArgs.StatefulSession)

	execCtx := protocol.WithProfileChecker(ctx, a)
	if a.perm.modes != nil { // 未装配权限判定时不挂授权器，桥退回只读幂等规则（fail-closed）
		execCtx = protocol.WithToolCallAuthorizer(execCtx, a)
	}
	caResult, err := a.codeAct.Execute(execCtx, CodeActRequest{
		Language:        lang,
		Code:            codeArgs.Code,
		CapabilityID:    jitTok.Claims.TokenID,
		SessionID:       targetSessionID,
		AgentID:         a.ID,
		StatefulSession: stateful,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: execute failed", err)
	}

	safeOut, err := taint.SanitizeToSafe(caResult.Output)
	if err != nil {
		pol := a.currentSecurityPolicy()
		if pol.CodeActTaintBlockEnabled {
			return nil, apperr.Wrap(apperr.CodeForbidden, "code_act: taint check failed", err)
		}
		// full 档自动放行：不阻断，但必须留审计与指标（V9 / HE-1，R5-04）
		a.recordCodeActTaintBypass(ctx, pol.Profile, err)
	}

	var outputBytes []byte
	if err == nil {
		outputBytes = []byte(safeOut.IntoMessage("").Content)
	} else {
		outputBytes = []byte(caResult.Output.UnsafeContent())
	}

	return &types.ToolResult{
		Output:     outputBytes,
		Success:    caResult.ExitCode == 0,
		LatencyMs:  caResult.LatencyMs,
		TaintLevel: caResult.Output.Level(),
	}, nil
}

// recordCodeActTaintBypass 记录 full 档放行 code_act 污点清洗失败的输出：
// 与 tool_policy.go 的 anomaly 放行同口径——slog 告警 + 审计存储 + hitl_auto_approved_total。
func (a *Agent) recordCodeActTaintBypass(ctx context.Context, profile policy.TrustProfile, cause error) {
	slog.WarnContext(ctx, "agent: code_act taint check failed, auto-approved (full profile)",
		"agent_id", a.ID, "profile", profile, "err", cause)
	metrics.RecordHITLAutoApproved(ctx, "code_act", "code_act_taint")
	if a.Security.TaintAuditor == nil {
		return
	}
	meta := map[string]any{
		"tool":            "code_act",
		"checkpoint_type": "code_act_taint",
		"profile":         string(profile),
		"reason":          cause.Error(),
		"agent_id":        a.ID,
	}
	if a.sCtx != nil && a.sCtx.SessionID != "" {
		meta["session_id"] = a.sCtx.SessionID
	}
	if err := a.Security.TaintAuditor.Log(ctx, "hitl_auto_approved", meta); err != nil {
		slog.ErrorContext(ctx, "agent: failed to log code_act taint auto approved audit", "err", err)
	}
}

// codeActToolSchema 返回下发给模型的 code_act 工具定义（R3-02）。
func codeActToolSchema() types.ToolSchema {
	return types.ToolSchema{
		Name:        "code_act",
		Description: "Run a Python or Bash script in the sandbox. In Python, polaris.call(name, **args) calls a registered tool (loaded or not; each call is authorized like a direct call, and calls that would need user approval are refused — make those outside code_act) and returns its result; polaris.call_many([...]) runs calls concurrently; polaris.tools(query) lists callable tools. Only what the script prints is returned to you. Use it for multi-step data work so intermediate results stay out of your context.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"enum":        []string{"python", "bash"},
					"description": "Script language.",
				},
				"code": map[string]any{
					"type":        "string",
					"description": "Script source.",
				},
				"session_id": map[string]any{
					"type":        "string",
					"description": "Optional; reuse to keep interpreter state across calls.",
				},
			},
			"required": []string{"language", "code"},
		},
	}
}

// resolveCodeActSessionID 解析并规范化 code_act 会话键与持久化状态标记（R4-03）。
func (a *Agent) resolveCodeActSessionID(sessionID string, statefulSession bool) (string, bool) {
	agentSessionID := ""
	if a.sCtx != nil {
		agentSessionID = a.sCtx.SessionID
	}
	if agentSessionID == "" {
		agentSessionID = a.ID
	}

	stateful := statefulSession
	targetSessionID := agentSessionID
	if sessionID != "" {
		stateful = true
		cleanSub := filepath.Base(filepath.Clean(sessionID))
		if cleanSub == "" || cleanSub == "." || cleanSub == string(filepath.Separator) {
			cleanSub = "default"
		}
		if agentSessionID != "" {
			targetSessionID = agentSessionID + ":" + cleanSub
		} else {
			targetSessionID = cleanSub
		}
	}
	return targetSessionID, stateful
}
