package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func canvasResultSummary(res *types.ToolResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if res == nil {
		return ""
	}
	if !res.Success {
		if res.Error != "" {
			return res.Error
		}
		return "failed"
	}
	return string(res.Output)
}

//nolint:gocyclo // MVP intercept logic
func (a *Agent) interceptComputerUse(ctx context.Context, toolName string, args []byte) error {
	if toolName != "computer_use" && toolName != "browser_use" {
		return nil
	}

	// Cedar 策略预检（R3）：deny-by-default，先于 HITL 审批。
	// LAM engine 为 nil 时跳过（保持与无 LAM 场景兼容）。
	if a.lamEngine != nil {
		if pErr := a.lamEngine.CheckPolicy(ctx, args); pErr != nil {
			return pErr //nolint:wrapcheck
		}
	}

	mode := "auto_review"
	if a.sCtx.Preferences != nil {
		if v, ok := a.sCtx.Preferences["computer_use_mode"]; ok && v != "" {
			mode = v
		}
	}

	isDangerous := false
	switch toolName {
	case "computer_use":
		var actionReq struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &actionReq)
		if actionReq.Action == "key" || actionReq.Action == "type" || actionReq.Action == "left_click" || actionReq.Action == "right_click" || actionReq.Action == "double_click" || actionReq.Action == "left_click_drag" {
			isDangerous = true
		}
	case "browser_use":
		var actionReq struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &actionReq)
		if actionReq.Action == "click" || actionReq.Action == "type" || actionReq.Action == "key" {
			isDangerous = true
		}
	}

	needHITL := false
	if mode == "default" {
		needHITL = true
	} else if mode == "auto_review" && isDangerous {
		needHITL = true
	}

	if needHITL && a.hitl != nil {
		prompt := types.HITLPrompt{
			ID:             uuid.New().String(),
			AgentID:        a.sCtx.AgentID,
			CheckpointType: types.CheckpointDeviceControlReview,
			PromptText:     fmt.Sprintf("Agent requests to execute %s with args: %s\nMode: %s", toolName, string(args), mode),
			Options: []types.HITLOption{
				{Key: "approve", Label: "Approve"},
				{Key: "deny", Label: "Deny"},
			},
			DeadlineNs: time.Now().Add(5 * time.Minute).UnixNano(),
			// PermissionMode 供 resolveTimeoutAction 区分：仅 full_access 下超时
			// 才允许兜底为 auto_approve，与"设置 → 设备操控"承诺的语义一致。
			PermissionMode: types.PermissionMode(mode),
		}
		respHITL, hitlErr := a.hitl.Prompt(ctx, prompt)
		if hitlErr != nil || respHITL == nil || respHITL.OptionKey != "approve" {
			if hitlErr != nil {
				return apperr.Wrap(apperr.CodeForbidden, "HITL gateway denied computer use action", hitlErr)
			}
			return apperr.New(apperr.CodeForbidden, "HITL gateway denied computer use action")
		}
	}
	return nil
}

// extractTaskType 从任务目标字符串提取规范化任务类型键。
// 2026-07-21 deadcode 审查去重：此前与 prompt.ExtractTaskType 是逐字节相同的
// 平行实现，注释所称"避免 L1 到 L2 的依赖"已过期——internal/prompt/optimizer 现属
// L1（与 internal/agent 同层，非 L2），且 optimizer 不反向依赖 agent，无循环风险。
// prompt.ExtractTaskType 有 internal/eval/control 的 V8-S4 确定性不变量测试覆盖，
// 是两者中被正式验证的一份，故改为委托，消除漂移风险（两份实现未来可能各自被
// 修改而不同步，是比重复代码本身更大的隐患）。
func extractTaskType(goal string) string {
	return prompt.ExtractTaskType(goal)
}

// withTaskScopeCtx 把当前会话标识注入 ctx，供 tokenizeMessagesForLLM 写令牌、
// internal/tool/tool.go ExecuteTool 与 execute/dag/executor.go DAGExecutor.Execute 还原令牌时
// 使用同一 taskID 命名空间（M11 §5.1 PII OpaqueToken 任务级隔离）。
//
// 必须使用 a.sCtx.SessionID，不能用 a.sCtx.TaskID——二者是不同字段：TaskID 是
// 当前认领的 Blackboard task_id，由 Worker 在每次 Run() 前通过 SetTaskID() 注入，
// 会随会话内认领的任务变化；SessionID 贯穿整个 Agent 会话生命周期不变，且是仓库
// 既有惯例里传给 protocol.CtxTaskIDKey 的值（见 fsm/state_machine.go §422-423
// 注释、agent_execute_dag.go 里 a.dagRunner.Run(ctx, plan, toolExecFn, nil, a.sCtx.SessionID, a.sCtx.AgentID)
// 调用点）。写入令牌与还原令牌若使用不同字段，会导致同一次调用链前后用不上同一个
// taskID 命名空间，隔离和清理都会失效。
//
// SessionID 为空（例如脱离 Agent 生命周期的单元测试直接构造 Agent{} 场景）时不做
// 任何注入，保留调用方传入的 ctx 原样，交由 tokenizeMessagesForLLM 内部继续用空
// taskID 兜底（等价于 legacy Tokenize/Resolve/Restore 路径）。
func (a *Agent) withTaskScopeCtx(ctx context.Context) context.Context {
	if a.sCtx != nil && a.sCtx.SessionID != "" {
		ctx = context.WithValue(ctx, protocol.CtxTaskIDKey{}, a.sCtx.SessionID)
	}
	// anomalyFilter 恒非 nil（NewAgent 默认构造，见 agent.go），随任务域 ctx 一并
	// 注入，供 internal/tool/tool.go checkAnomaly 读取（ADR-0062 关联接线）。
	if a.Security.AnomalyFilter != nil {
		ctx = context.WithValue(ctx, protocol.CtxAnomalyFilterKey{}, a.Security.AnomalyFilter)
	}
	return ctx
}

// tokenizeMessagesForLLM 在消息送入 LLM Provider 前，对每条 message.Content 做 PII 令牌化。
// piiDetector/tokenVault 任一为 nil 时直接跳过（不阻塞主流程，Tier0 无 Presidio 场景下的降级行为）。
func (a *Agent) tokenizeMessagesForLLM(ctx context.Context, messages []types.Message) ([]types.Message, error) {
	if a.Security.PIIDetector == nil || a.Security.TokenVault == nil {
		return messages, nil
	}
	out := make([]types.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.Content == "" {
			continue
		}
		// RedactWithMode 会内部从 ctx 提取 CtxTaskIDKey 并调用 TokenizeForTask
		tokenized, n, err := a.Security.PIIDetector.RedactWithMode(ctx, m.Content, "tokenize", "", nil, a.Security.TokenVault)
		if err != nil {
			slog.Error("agent: PII tokenization failed, fail-closed", "err", err)
			// 选择 fail-closed 策略：如果 PII 提取失败，截断流程，防止明文 PII 流入 LLM。
			return nil, apperr.Wrap(apperr.CodeInternal, "tokenizeMessagesForLLM", err)
		}
		if n > 0 {
			out[i].Content = tokenized
		}
	}
	return out, nil
}
