package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// D5（GD-14-004）→ GD-1（本轮升级，local_playground/upgrade/01-架构设计变更规范.md）：
// 工具化 Multi-Agent Handoff，异步挂起版本。
//
// 历史背景：原实现（D5 首版）为同步阻塞——`transfer_to_agent` 工具调用内部
// 轮询等待子任务终态，独占一个 DAG 执行槽位直至委派完成。GD-1 复核后改为
// 异步挂起：FSM 新增 S_AWAIT_AGENT 状态（见 docs/arch/spec/state.yaml），
// `executeTransferToAgent` 首次调用时投递子任务后立即返回
// `ToolResult{Suspended:true}` 并推进 TriggerAwaitAgent，不再阻塞当前
// goroutine。真正的恢复由 `watchHandoffCompletion` 启动的后台 watcher
// 完成（见本文件下方），完成后投递 TriggerAgentHandoffDone 使 FSM 回到
// S_EXECUTE，`runExecuteDAG` 重新进入本函数时，`a.sCtx.HandoffTaskID` 非空
// 触发"恢复检查"分支，直接返回子任务结果，不重新投递任务。
//
// 崩溃安全边界（诚实声明，非本次范围）：watcher 是绑定 `a.ctx`（Agent 进程
// 内存态）生命周期的 goroutine，与 spawn_planner 现有的 whisperChan 方案
// 风险等级一致——若进程在委派等待期间重启，watcher 随进程消亡。
// `a.taskCheckpointRepo` 持久化的 checkpoint 记录为未来实现"独立
// Reconciler 扫描 task_checkpoints 补偿唤醒"预留了锚点，但该 Reconciler
// 本次未实现。这不是本次修复引入的新缺陷，而是与既有 whisperChan 机制
// 相同量级的既有限制，留作独立后续工作。

// InjectHandoffPoster 注入 transfer_to_agent 工具所需的 Blackboard 任务投递能力
// （D5/GD-14-004）。nil 时 transfer_to_agent 节点返回 fail-closed 错误，不影响
// 其余工具执行路径。注入器放在本文件而非 agent_wiring.go——后者已逼近 R7 的
// 400 行上限，新增注入逻辑归入功能更内聚的本文件更合适。
func (a *Agent) InjectHandoffPoster(p HandoffPoster) {
	a.handoffPoster = p
}

// executeTransferToAgent 处理 transfer_to_agent 工具调用：
//  1. 以当前任务的 NamespaceID（GD-14-001 共享记忆命名空间）为目标角色创建一条
//     新的 Blackboard Task（Type 编码为 "agent_handoff:<role>"，供目标 Worker
//     按前缀路由认领）。
//  2. 阻塞轮询等待该 Task 到达终态（Done/Failed）或 ctx 超时。
//  3. 将结果包装为 ToolResult 返回给 DAG 执行器。
//
// taintLevel 必须是调用方（runExecuteDAG）传入的、已经过 S_VALIDATE 四层校验
// 的授信值，禁止从 LLM 生成的工具参数中采信（与 F0-2/GD-04-001 同一不变量）。
func (a *Agent) executeTransferToAgent(ctx context.Context, targetRole, contextSummary string, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	if a.handoffPoster == nil {
		return nil, apperr.New(apperr.CodeInternal,
			"transfer_to_agent: handoffPoster not injected (fail-closed)")
	}
	if targetRole == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "transfer_to_agent: target_agent_role is required")
	}

	namespace := a.sCtx.NamespaceID
	if namespace == "" {
		// 未配置共享命名空间时退化为以当前 SessionID 作为命名空间，保证委派
		// 任务至少与发起方共享同一记忆检索范围（不引入全新隔离域）。
		namespace = a.sCtx.SessionID
	}

	// [GD-1] 检查是否为挂起恢复
	if a.sCtx.HandoffTaskID != "" {
		snap, err := a.handoffPoster.PeekTask(ctx, a.sCtx.HandoffTaskID)
		if err == nil && snap != nil && snap.Status == types.TaskDone {
			a.sCtx.HandoffTaskID = "" // 清理恢复状态
			return &types.ToolResult{
				Success:    true,
				Output:     snap.Result,
				TaintLevel: taintLevel,
			}, nil
		}
		if err == nil && snap != nil && snap.Status == types.TaskFailed {
			a.sCtx.HandoffTaskID = "" // 清理恢复状态
			return &types.ToolResult{ //nolint:nilerr
				Success: false,
				Output:  []byte(fmt.Sprintf("handoff task %s failed", snap.ID)),
			}, nil
		}
		// 如果还没完成，或者查不到（异常），继续往下或者直接再次挂起
		return &types.ToolResult{
			Success:    true,
			Suspended:  true, // 告知 DAG 执行器中断
			Output:     []byte("Agent suspended waiting for handoff task completion."),
			TaintLevel: taintLevel,
		}, nil
	}

	childID := fmt.Sprintf("handoff-%s-%s", a.ID, uuid.NewString())
	entry := &types.TaskEntry{
		ID:          childID,
		Type:        "agent_handoff:" + targetRole,
		Priority:    5,
		Intent:      []byte(contextSummary),
		IntentTaint: taintLevel,
		Namespace:   namespace,
	}

	if err := a.handoffPoster.PostTask(ctx, entry); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "transfer_to_agent: post task", err)
	}

	// [GD-1] 改造为异步挂起，移除阻塞轮询
	a.sCtx.HandoffTaskID = childID
	a.asyncIntent(types.TriggerAwaitAgent)

	return &types.ToolResult{
		Success:    true,
		Suspended:  true, // 告知 DAG 执行器中断
		Output:     []byte("Agent suspended waiting for handoff task completion."),
		TaintLevel: taintLevel,
	}, nil
}

// handoffWatchPollInterval 委派完成后台轮询间隔。后台 watcher 不再占用 DAG
// 执行槽位，无需追求原同步阻塞轮询的 500ms 低延迟，1s 足够。
const handoffWatchPollInterval = 1 * time.Second

// watchHandoffCompletion 启动一个绑定 a.ctx（Agent 生命周期）的后台
// goroutine，轮询委派子任务 childTaskID 的终态；到达终态后异步投递
// TriggerAgentHandoffDone 唤醒 FSM（S_AWAIT_AGENT → S_EXECUTE）。
//
// [GD-1 修复] 此前版本在进入 S_AWAIT_AGENT 后错误地投递了
// TriggerSuspend——该 Trigger 在 FSM 转移表中只定义了
// S_IDLE → S_SUSPENDED 一条边，从 S_AWAIT_AGENT 触发会命中
// state_machine.go Dispatch() 的"no transition from %v with trigger %v"
// 硬错误分支，导致 Agent.Run() 直接返回错误退出——每次 transfer_to_agent
// 调用都会使父任务在进入等待状态后立即失败，且没有任何组件会在委派完成
// 时唤醒它（全仓搜索确认不存在对应的常驻 Watcher）。本函数是该缺失环节的
// 补齐：不再投递任何 TriggerSuspend，FSM 保持在 S_AWAIT_AGENT 自然等待
// 下一个外部 Trigger（与其它非终态状态的等待方式一致），由本 watcher
// 负责在委派完成时把 TriggerAgentHandoffDone 送入 a.intent。
func (a *Agent) watchHandoffCompletion(childTaskID string) {
	if a.handoffPoster == nil || childTaskID == "" {
		return
	}
	concurrent.SafeGo(a.ctx, "agent.handoff_watcher", func(ctx context.Context) {
		ticker := time.NewTicker(handoffWatchPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snap, err := a.handoffPoster.PeekTask(ctx, childTaskID)
				if err != nil {
					slog.Warn("agent: handoff watcher peek failed", "task_id", childTaskID, "err", err)
					continue
				}
				if snap == nil {
					continue
				}
				if snap.Status == types.TaskDone || snap.Status == types.TaskFailed {
					a.asyncIntent(types.TriggerAgentHandoffDone)
					return
				}
			}
		}
	})
}
