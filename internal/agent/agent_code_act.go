package agent

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runCodeAct 执行代码执行沙箱节点，staged 与 agentic 模式共用（R3-02）。
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

	sessionID := ""
	if a.sCtx != nil {
		sessionID = a.sCtx.SessionID
	}

	caResult, err := a.codeAct.Execute(protocol.WithProfileChecker(ctx, a), CodeActRequest{
		Language:        lang,
		Code:            codeArgs.Code,
		CapabilityID:    jitTok.Claims.TokenID,
		SessionID:       sessionID,
		AgentID:         a.ID,
		StatefulSession: codeArgs.StatefulSession,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act: execute failed", err)
	}

	return &types.ToolResult{
		Output:     []byte(caResult.Output.UnsafeContent()),
		Success:    caResult.ExitCode == 0,
		LatencyMs:  caResult.LatencyMs,
		TaintLevel: caResult.Output.Level(),
	}, nil
}

// codeActToolSchema 返回下发给模型的 code_act 工具定义（R3-02）。
func codeActToolSchema() types.ToolSchema {
	return types.ToolSchema{
		Name:        "code_act",
		Description: "在沙箱中执行 Python 或 Bash 脚本。可以用 polaris.call() / polaris.call_many() 调用其他工具，只有 print 输出会返回。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type":        "string",
					"enum":        []string{"python", "bash"},
					"description": "执行脚本的语言环境 (python 或 bash)",
				},
				"code": map[string]any{
					"type":        "string",
					"description": "要执行的脚本代码内容",
				},
				"session_id": map[string]any{
					"type":        "string",
					"description": "可选的状态会话ID，用于跨调用共享执行上下文",
				},
			},
			"required": []string{"language", "code"},
		},
	}
}
