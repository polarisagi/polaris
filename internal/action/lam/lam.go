package lam

import (
	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/prompt/templates"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxScreenshotBytesFull Tier 1+ vision 路径截图上限（超出则降级 DOM-only）。
const maxScreenshotBytesFull = 2 * 1024 * 1024 // 2MB

// ScreenState 屏幕多模态状态表示。
type ScreenState struct {
	ScreenshotBytes []byte // 原始 PNG 图像；超出 maxScreenshotBytesFull 时 VLM 路径自动降级为 DOM-only
	Width           int
	Height          int
	DOM             string // 无障碍 DOM 树（文本格式），Tier 0 唯一语境来源
}

// ExecutorFn 是底层 GUI 执行函数签名，对应 action.ComputerUseTool.Execute。
// 由上层注入，避免 lam → action 包循环依赖。
type ExecutorFn func(ctx context.Context, input []byte) ([]byte, error)

// LAMConfig 大动作模型配置。
type LAMConfig struct {
	Enabled       bool
	ResolverModel string // VLM 动作解析模型，e.g. "deepseek-chat"（budget 层）
}

// ComputerUseEngine 实现 LargeActionModel：intent + ScreenState → VLM → action → 执行。
type ComputerUseEngine struct {
	config    LAMConfig
	provider  protocol.Provider
	executor  ExecutorFn // 注入 action.NewComputerUseTool().Execute；nil 时为 dry-run 模式
	policy    protocol.PolicyGate
	streamBus *StreamingActionBus // [Task 3] 动作流总线
}

// NewComputerUseEngine 构造 ComputerUseEngine。
// executor 由调用方注入（通常为 action.NewComputerUseTool().Execute），
// 解耦 lam 子包与 action 父包，便于单元测试注入 mock。
func NewComputerUseEngine(cfg LAMConfig, provider protocol.Provider, executor ExecutorFn, policy protocol.PolicyGate, streamBus *StreamingActionBus) *ComputerUseEngine {
	return &ComputerUseEngine{
		config:    cfg,
		provider:  provider,
		executor:  executor,
		policy:    policy,
		streamBus: streamBus,
	}
}

// StreamBus 返回动作流总线，供 M4 S_EXECUTE 路由鼠标/键盘连续动作。
func (e *ComputerUseEngine) StreamBus() *StreamingActionBus {
	return e.streamBus
}

// ExecuteAction 将自然语言意图转为 GUI 动作并执行。
// 路径：
//   - Tier 0 / DOM-only：截图不发 VLM，以 DOM 文本驱动动作解析（低内存开销）
//   - Tier 1+ / vision：截图 ≤ 2MB 时随 DOM 一并提交 VLM
func (e *ComputerUseEngine) ExecuteAction(ctx context.Context, intent string, screenState *ScreenState) (*types.ToolResult, error) {
	if !e.config.Enabled {
		return &types.ToolResult{Success: false, Error: "Computer Use is disabled (LAMConfig.Enabled=false)"}, nil
	}

	// 硬件门控：FeatureComputerUseGUI 未启用时拒绝执行
	if fg := probe.GlobalFeatureGate(); fg != nil && !fg.IsEnabled(probe.FeatureComputerUseGUI) {
		return &types.ToolResult{
			Success: false,
			Error:   "FeatureComputerUseGUI not enabled: requires display + 512MB RAM",
		}, nil
	}

	if e.provider == nil {
		return nil, apperr.New(apperr.CodeInternal, "lam: provider not injected")
	}
	if screenState == nil {
		return nil, apperr.New(apperr.CodeInternal, "lam: screenState is nil")
	}

	// 截图超出上限时降级为 DOM-only（保护 Tier 0 内存预算）
	useVision := len(screenState.ScreenshotBytes) > 0 &&
		len(screenState.ScreenshotBytes) <= maxScreenshotBytesFull

	actionJSON, err := e.resolveAction(ctx, intent, screenState, useVision)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ComputerUseEngine.ExecuteAction", err)
	}

	// 解析 actionJSON 以进行权限控制
	if err := e.checkPolicy(ctx, actionJSON); err != nil {
		return nil, err
	}

	// dry-run 模式（executor 未注入）：返回解析的动作 JSON 供调试
	if e.executor == nil {
		return &types.ToolResult{Success: true, Output: actionJSON, TaintLevel: types.TaintHigh}, nil
	}

	out, execErr := e.executor(ctx, actionJSON)
	if execErr != nil {
		return &types.ToolResult{Success: false, Error: execErr.Error(), TaintLevel: types.TaintHigh}, nil //nolint:nilerr
	}
	return &types.ToolResult{Success: true, Output: out, TaintLevel: types.TaintHigh}, nil
}

// CheckPolicy 导出 Cedar 策略检查，供 Agent Kernel 在 interceptComputerUse 时调用。
// 入参 actionJSON 为待执行 GUI 动作的 JSON 字节（与 ExecuteAction 内部格式一致）。
func (e *ComputerUseEngine) CheckPolicy(ctx context.Context, actionJSON []byte) error {
	return e.checkPolicy(ctx, actionJSON)
}

func (e *ComputerUseEngine) checkPolicy(ctx context.Context, actionJSON []byte) error {
	if e.policy == nil {
		return apperr.New(apperr.CodeForbidden, "lam: policy gate not initialized (deny-by-default)") // fail-closed，勿 nil 放行
	}
	var actionMap map[string]any
	if err := json.Unmarshal(actionJSON, &actionMap); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "lam: parse action", err)
	}
	actionType, _ := actionMap["action"].(string)
	allowed, pErr := e.policy.IsAuthorized(ctx, "agent", "browser_automate", "lam",
		map[string]any{"allow_net": true, "action": actionType, "trust_tier": int(types.TrustLocal)})
	if pErr != nil || !allowed {
		reason := "policy denied"
		if pErr != nil {
			reason = pErr.Error()
		}
		return apperr.New(apperr.CodeForbidden, "lam: browser_automate denied: "+reason)
	}
	return nil
}

// vlmActionOutput VLM 响应的结构化动作。
type vlmActionOutput struct {
	Action     string `json:"action"`
	Coordinate []int  `json:"coordinate,omitempty"`
	Text       string `json:"text,omitempty"`
	Reasoning  string `json:"reasoning,omitempty"` // 仅用于日志，不转发给 executor
}

// resolveAction 调用 VLM 将意图解析为具体 GUI 动作 JSON。
func (e *ComputerUseEngine) resolveAction(ctx context.Context, intent string, state *ScreenState, useVision bool) ([]byte, error) {
	var userContent string
	var userMsg types.Message

	if useVision {
		// Tier 1+ vision 路径：注明分辨率，图片编码通过 types.Message.Parts 传递。
		userContent = fmt.Sprintf(
			"屏幕分辨率：%dx%d\nDOM 结构：\n%s\n\n用户意图：%s",
			state.Width, state.Height, state.DOM, intent,
		)
		b64 := base64.StdEncoding.EncodeToString(state.ScreenshotBytes)
		userMsg = types.Message{
			Role:    "user",
			Content: userContent,
			Parts: []any{
				map[string]any{"type": "text", "text": userContent},
				map[string]any{
					"type": "image_url",
					"image_url": map[string]string{
						"url": "data:image/png;base64," + b64,
					},
				},
			},
		}
	} else {
		// Tier 0 DOM-only 路径：纯文本驱动，零图片 token 消耗
		userContent = fmt.Sprintf("DOM 结构：\n%s\n\n用户意图：%s", state.DOM, intent)
		userMsg = types.Message{
			Role:    "user",
			Content: userContent,
		}
	}

	// Prompt 统一走 internal/prompt/templates 管理（A-12 修复）；系统提示词为纯静态
	// 文本，不拼接任何不可信输入，不需要边界符防护。
	systemPrompt, tmplErr := templates.Render("lam_gui_resolver_system.tmpl", nil)
	if tmplErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "lam: render resolver system prompt", tmplErr)
	}

	req := &types.InferRequest{
		Model: e.config.ResolverModel,
		Messages: []types.Message{
			{
				Role:    "system",
				Content: systemPrompt,
			},
			userMsg,
		},
		MaxTokens:   256,
		Temperature: 0, // 动作解析需要确定性
		ResponseFormat: &types.ResponseFormat{
			Type: "json_schema",
			JSONSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type": "string",
						"enum": []string{"screenshot", "left_click", "right_click", "mouse_move", "type", "key"},
					},
					"coordinate": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
					"text":       map[string]any{"type": "string"},
					"reasoning":  map[string]any{"type": "string"},
				},
				"required": []string{"action"},
			},
		},
	}

	// P-1：每次 LLM 调用自持超时（90s），不信任上层 ctx 一定带 deadline（A-05）。
	inferCtx, inferCancel := context.WithTimeout(ctx, 90*time.Second)
	defer inferCancel()
	resp, err := safecall.Infer(inferCtx, e.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("lam: VLM resolve action: %v", err), err)
	}

	var out vlmActionOutput
	if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("lam: parse VLM output: %v", err), err)
	}
	if out.Action == "" {
		return nil, apperr.New(apperr.CodeInternal, "lam: VLM returned empty action")
	}

	// 重新序列化：去掉 reasoning，只传 executor 需要的字段
	type execArgs struct {
		Action     string `json:"action"`
		Coordinate []int  `json:"coordinate,omitempty"`
		Text       string `json:"text,omitempty"`
	}
	return json.Marshal(execArgs{
		Action:     out.Action,
		Coordinate: out.Coordinate,
		Text:       out.Text,
	})
}
