package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// stateToTriggerMap 将下层 Effect 产生的文本 State 映射回 FSM 驱动所需的 AgentTrigger。
// READ-ONLY: 返回的 map 在调用方不得修改。
func stateToTriggerMap() map[types.State]types.AgentTrigger {
	return map[types.State]types.AgentTrigger{
		"S_PERCEIVE_DONE":   types.TriggerPerceiveDone,
		"S_PERCEIVE_FAILED": types.TriggerReplanExhausted, // 早期失败直接熔断
		"S_PLAN_DONE":       types.TriggerPlanDone,
		"S_PLAN_FAILED":     types.TriggerReplanExhausted,
		"S_VALIDATE_OK":     types.TriggerValidateOk,
		"S_VALIDATE_FAIL":   types.TriggerValidateFail,
		"S_EXECUTE_OK":      types.TriggerExecuteDone,
		"S_EXECUTE_FAIL":    types.TriggerExecuteFail,
		"S_REPLAN_DONE":     types.TriggerReplanDone,
		"S_REPLAN_FAILED":   types.TriggerReplanExhausted,
		"S_REFLECT_DONE":    types.TriggerReflectDone,
		"S_REFLECT_FAILED":  types.TriggerReplanExhausted,
		"S_ROLLBACK_OK":     types.TriggerRollbackDone,
	}
}

// toProtocolCtx 内部助手：映射内部状态至协议状态，并在此时提权计算最大污点，防止 Taint Washing。
// 供 Effect 执行使用：LLMFillEffect 调 LLM 后走 OnSuccess/OnFailure 推进状态；DeterministicEffect 调用纯函数。
func (a *Agent) toProtocolCtx() protocol.StateContext {
	maxTaint := types.TaintNone
	if a.sCtx != nil {
		maxTaint = a.sCtx.GlobalTaintLevel
		if lv := a.sCtx.RawIntentTS.Level(); lv > maxTaint {
			maxTaint = lv
		}
	}
	return protocol.StateContext{
		AgentID:              a.ID,
		SessionID:            a.sCtx.SessionID,
		MaxTaintLevel:        maxTaint,
		Mem:                  a.memory,
		Tools:                a.toolRegistry,
		Provider:             a.provider,
		Policy:               a.policyGate,
		Preferences:          a.sCtx.Preferences,
		SagaLog:              a.sCtx.SagaLog,
		InitialMaxStepsLimit: a.sCtx.InitialMaxStepsLimit,
	}
}

// Interrupt 向 Agent 发送中断请求（非阻塞，inv_global_08 <200ms SLO）。
// Resume → 恢复原状态；Redirect → 更新意图后恢复（重新规划）；Abort → S_FAILED。
//
// GR-4-004 修复：本方法由外部 goroutine 调用，与 Agent.Run() 主循环并发运行。
// 原实现直接写 a.sCtx.InterruptReq 和 a.sCtx.RawIntentTS，存在数据竞争：
//   - InterruptReq 是死字段（全仓库仅此一处写，从未被任何代码读取），直接删除。
//   - RawIntentTS 改为通过 pendingRedirectCh（容量 1）传递给主循环在单线程内安全写入。
//
// channel 发送用 select/default：如果主循环还未消费上一条 redirect，则新值覆盖
// （丢弃旧值后重新放入），保证最后一次 Redirect 意图生效。
func (a *Agent) Interrupt(req types.InterruptRequest) {
	switch req.Action {
	case types.InterruptRedirect:
		// 将重定向意图字符串投递到 channel，由主循环在单线程内安全写入 sCtx.RawIntentTS。
		if req.Redirect != "" {
			select {
			case a.pendingRedirectCh <- req.Redirect:
			default:
				// channel 已满（上一条 redirect 未消费），弹出旧值放入新值
				select {
				case <-a.pendingRedirectCh:
				default:
				}
				a.pendingRedirectCh <- req.Redirect
			}
		}
		_ = a.SendIntent(types.TriggerInterruptReceived)
		// 注入到 S_INTERRUPT 后立即 Resume（Redirect = 新意图的 Resume）
		a.asyncIntent(types.TriggerInterruptResume)
	case types.InterruptAbort:
		_ = a.SendIntent(types.TriggerInterruptReceived)
		a.asyncIntent(types.TriggerInterruptAbort)
	default: // types.InterruptResume
		_ = a.SendIntent(types.TriggerInterruptReceived)
		a.asyncIntent(types.TriggerInterruptResume)
	}
}

// refreshInstalledExtensions 从 extension_instances 表动态查询已安装扩展并存入 fsm.StateContext。
func (a *Agent) refreshInstalledExtensions(ctx context.Context) {
	if a.catalog == nil {
		a.sCtx.InstalledExtensionsInfo = ""
		return
	}

	entries := a.catalog.List(ctx, types.TrustUntrusted)
	var exts []string
	for _, e := range entries {
		switch e.Source {
		case types.ToolMCP:
			exts = append(exts, fmt.Sprintf("- [MCP] %s: %s", e.MCPServerID, e.Name))
		case types.ToolSkill:
			exts = append(exts, fmt.Sprintf("- [Skill] %s", e.Name))
		}
	}

	if len(exts) > 0 {
		a.sCtx.InstalledExtensionsInfo = "Installed Extensions:\n" + strings.Join(exts, "\n")
	} else {
		a.sCtx.InstalledExtensionsInfo = ""
	}
}

// InjectExtensionActivator 注入按需扩展激活器。
func (a *Agent) InjectExtensionActivator(activator fsm.ExtensionActivatorIface) {
	a.sm.WithExtensionActivator(activator)
}

// InjectReplanExtensionActivationTimeout 注入 S_REPLAN 扩展激活 Effect 的超时上限
// （state.yaml §thresholds replan_extension_activation_s）。
func (a *Agent) InjectReplanExtensionActivationTimeout(d time.Duration) {
	a.sm.WithReplanExtensionActivationTimeout(d)
}

type agentContextBuilder struct {
	cata catalog.Catalog
}

func (b *agentContextBuilder) BuildPerceiveContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	msgs, err := agentctx.BuildPerceiveContext(ctx, memory, sCtx, cognitive)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "agentContextBuilder.BuildPerceiveContext", err)
	}
	return msgs, nil
}

func (b *agentContextBuilder) BuildPlanContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cata catalog.Catalog, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	useCata := cata
	if useCata == nil {
		useCata = b.cata
	}
	msgs, err := agentctx.BuildPlanContext(ctx, memory, sCtx, useCata, cognitive)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "agentContextBuilder.BuildPlanContext", err)
	}
	return msgs, nil
}

func (b *agentContextBuilder) BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
	msgs, err := agentctx.BuildReflectContext(ctx, memory, sCtx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "agentContextBuilder.BuildReflectContext", err)
	}
	return msgs, nil
}

func (b *agentContextBuilder) BuildToolListSection(ctx context.Context, cata catalog.Catalog) string {
	useCata := cata
	if useCata == nil {
		useCata = b.cata
	}
	return agentctx.BuildToolListSection(ctx, useCata)
}

func (a *Agent) InjectTerminalCallback(cb func(ctx context.Context, taskID, taskType string, replanCount int, success bool)) {
	a.terminalCallback = cb
}

func (a *Agent) handleTerminalState(ctx context.Context, current types.AgentState) {
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:    types.AgentStreamEventStatus,
		Content: "task_done",
	})

	// M3 埋点：任务终态记录（驱动 polaris_task_success_rate）
	trace.RecordTaskOutcome(ctx, current == types.AgentStateComplete)

	// 接入运行时质量漂移检测（M03 §10.1）
	score := 1.0
	if current == types.AgentStateFailed {
		score = 0.0
	}
	metrics.GlobalPerformanceDrift().Record(score)

	// M4 §8：终态 PII 清零——SecureZero 删除 DB 快照，防止 PII 留存（M11 HE-Rule-2）
	if a.piiVault != nil && a.sCtx.TaskID != "" {
		if zeroErr := a.piiVault.SecureZero(ctx, a.sCtx.TaskID); zeroErr != nil {
			slog.Warn("agent: failed to secure zero PII vault", "err", zeroErr)
		}
	}
	// 清理键必须与 executeEffect 里 tokenizeMessagesForLLM 写入令牌时用的
	// ctx.Value(protocol.CtxTaskIDKey{}) 同一命名空间（a.sCtx.SessionID，
	// 不是 a.sCtx.TaskID——二者是不同字段，此处曾误用 TaskID 导致清理打不中
	// 实际写入的桶，见上方 SessionID 的既有说明）。
	if a.tokenVault != nil && a.sCtx != nil && a.sCtx.SessionID != "" {
		a.tokenVault.ClearTask(a.sCtx.SessionID)
	}

	// 触发 Terminal Callback (P1-2 Learning 闭环)。
	// 传 SessionID 而非 TaskID：ReflectionWorker 以此为键检索 episodic 事件
	// （事件写入时 TaskID 字段填的是 SessionID，见 executeEffect）。
	// ReplanCount 取状态机真实计数，供 MinReplanCount 门控。
	if a.terminalCallback != nil {
		sessionID := a.sCtx.SessionID
		if sessionID == "" {
			sessionID = a.sCtx.TaskID
		}
		a.terminalCallback(ctx, sessionID, "general", a.sm.ReplanCount(), current == types.AgentStateComplete)
	}
}
