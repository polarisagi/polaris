package fsm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// DeterministicEffect 函数——纯函数，重放时不重新调 LLM
// ============================================================================

func (sm *StateMachine) validateDAG(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
	// validateDAG 是纯函数存根，真正的四层校验通过 Agent.runValidateDAG 调用。
	// 这里返回 OK 是因为油门环节的真正输入（DAGModel + PolicyGate + TaintLevel）
	// 需要通过 Agent.sCtx 传递，所以该调用需要在带有完整 StateContext 的 Agent 上运行。
	// 在 DeterministicEffect.Fn 的签名限制下，我们返回占位状态；
	// 实际验证调用逻辑在 Agent.runValidateDAG 中。
	return types.State("S_VALIDATE_OK"), nil
}

func (sm *StateMachine) executeDAG(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
	if mem := sCtx.Mem; mem != nil {
		pressure := mem.GetMemoryPressure()
		if pressure.IsConstrained && pressure.AvailableMB < 100 {
			return types.State("S_EXECUTE_FAIL"), apperr.New(apperr.CodeResourceExhausted, "fsm: memory pressure too high, cannot proceed")
		}
	}
	// executeDAG 是纯函数存根。
	// 真正的执行在 Agent.runExecuteDAG 中，因为需要访问 a.toolRegistry。
	// S_EXECUTE 阶段拦截逻辑与 S_VALIDATE 相同，在 executeEffect 中进行。
	return types.State("S_EXECUTE_OK"), nil
}

func (sm *StateMachine) rollbackSaga(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
	var firstErr error
	// Saga 逆序补偿——已执行步骤的 Undo 操作
	for i := len(sCtx.SagaLog) - 1; i >= 0; i-- {
		step := sCtx.SagaLog[i]
		if step.UndoFn != "" && sCtx.Tools != nil {
			_, err := sCtx.Tools.ExecuteWithTaint(ctx, step.UndoFn, step.Args, sCtx.MaxTaintLevel)
			if err != nil {
				slog.Warn("Saga rollback failed for step", "node_id", step.NodeID, "tool", step.UndoFn, "err", err)
				if firstErr == nil {
					firstErr = apperr.Wrap(apperr.CodeInternal, "rollbackSaga: undo failed for node "+step.NodeID, err)
				}
			}
		}
	}
	if firstErr != nil {
		return types.State("S_ROLLBACK_PARTIAL"), firstErr
	}
	return types.State("S_ROLLBACK_OK"), nil
}

// ExtensionActivatorIface 消费方接口（防止包循环，定义在调用方）。
type ExtensionActivatorIface interface {
	FindAndActivate(ctx context.Context, goal string) ([]ExtActivatedHint, error)
}

// ExtActivatedHint 从扩展激活器传入 state_machine 的工具提示。
type ExtActivatedHint struct {
	ToolName    string
	Description string
}

func (sm *StateMachine) appendDynamicHints(msgs []types.Message) {
	sm.hintsMu.Lock()
	hints := sm.dynamicHints
	sm.hintsMu.Unlock()
	if len(hints) > 0 && len(msgs) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\n## 本次重规划新增可用工具\n")
		sb.WriteString("以下工具刚刚被激活，你可以在重规划中使用它们：\n")
		for _, h := range hints {
			fmt.Fprintf(&sb, "- **%s**: %s\n", h.ToolName, h.Description)
		}
		msgs[0].Content += sb.String()
	}
}

// WithExtensionActivator 注入按需扩展激活器（可选，启动时由上层 wire）。
func (sm *StateMachine) WithExtensionActivator(a ExtensionActivatorIface) {
	sm.activator = a
}

// WithReplanExtensionActivationTimeout 注入 S_REPLAN 扩展激活 Effect 的超时上限
// （state.yaml §thresholds replan_extension_activation_s，由启动装配层注入）。
// d<=0 时保留构造函数设置的安全默认值，不允许被配置成"无超时"。
func (sm *StateMachine) WithReplanExtensionActivationTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	sm.replanExtActivationTimeout = d
}

func (sm *StateMachine) shouldActivateExtensions(sCtx *StateContext) (goal string, should bool) {
	if sm.replanCount != 1 || sm.activator == nil {
		return "", false
	}
	if sCtx != nil && sCtx.TaskModel != nil {
		goal = sCtx.TaskModel.Goal
	}
	if goal == "" {
		return "", false
	}
	return goal, true
}
