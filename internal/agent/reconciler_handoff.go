package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// reconcileDrainTimeout 单个会话恢复等待子任务终态的上限。取值与
// scanInterval（5 分钟）同量级：委派子任务本身可能是一次完整的 Agent
// 轮次（LLM 调用 + 工具执行），远长于 boot_crash_recovery.go 的
// recoverySessionTimeout（2 分钟，仅覆盖单次 LLM 重放）。超时后必须
// release（归还 Pool 容量令牌），即便这会触发 pool.go release() 的
// 防御性 Interrupt(Abort)（GD-13-002 既有不变量，本组件不豁免）——
// 让父会话在这种极端情况下正常终止，好于无限期占用有限的 Pool 容量
// （Tier-0 HT0 仅 2-3 个并发 Agent 槽位）。
const reconcileDrainTimeout = 5 * time.Minute

// AwaitingHandoffReconciler 在进程重启后恢复处于 S_AWAIT_AGENT 态的会话。
// 数据已由 agent_execute_effect_helpers.go 落盘到 task_checkpoints，
// 本组件仅负责「读取 + 就位 FSM + 重挂 watcher + 等待终态后归还」。
//
// 设计要点（GD-13-003 系统级修复，见 local_playground/upgrade/05-design-GD-high-priority.md）：
//  1. Pool.Acquire 返回的全新 Agent 实例默认停在 S_IDLE，必须先用
//     Agent.ResumeAwaitingHandoff 就位到崩溃前的 S_AWAIT_AGENT，
//     随后投递的 TriggerAgentHandoffDone 才能命中转移表合法边。
//  2. pool.go release() 会对非终态 Agent 无条件下发 Interrupt(Abort)，
//     所以不能在会话到达终态前提前 release——必须复用 boot_crash_recovery.go
//     recoverOneSession 的「先订阅后触发、阻塞等待终态、有界超时」idiom。
//  3. 单个会话的等待通过 concurrent.SafeGo 放入独立 goroutine，避免一个
//     慢会话拖慢 Reconcile() 对其余候选会话的处理。
//  4. inFlight 去重：同一 sessionID 在前一次等待未结束前，周期性 Run()
//     的下一轮 Reconcile() 扫描到同一条 checkpoint 行时不重复 Acquire/
//     不重复挂 watcher（重复挂 watcher 本身不会崩溃——第二个 watcher
//     晚到的 asyncIntent 会被 Dispatch 拒绝并丢弃错误——但会造成多余的
//     Pool Acquire/release churn 与噪音日志，去重后更干净）。
type AwaitingHandoffReconciler struct {
	checkpointRepo protocol.TaskCheckpointRepository
	pool           protocol.AgentPool
	handoffPoster  HandoffPoster
	scanInterval   time.Duration
	// drainTimeout 同 reconcileDrainTimeout 默认值，拆成可覆盖字段仅为
	// 方便单测注入短超时（同包内测试直接赋值，不对外暴露 setter）。
	drainTimeout time.Duration

	inFlight sync.Map // sessionID(string) -> struct{}
}

func NewAwaitingHandoffReconciler(
	checkpointRepo protocol.TaskCheckpointRepository,
	pool protocol.AgentPool,
	handoffPoster HandoffPoster,
) *AwaitingHandoffReconciler {
	return &AwaitingHandoffReconciler{
		checkpointRepo: checkpointRepo,
		pool:           pool,
		handoffPoster:  handoffPoster,
		scanInterval:   5 * time.Minute,
		drainTimeout:   reconcileDrainTimeout,
	}
}

// Run 以 scanInterval 为周期持续调用 Reconcile，直到 ctx 被取消。
// 启动后立即先执行一次（不等第一个 tick），语义与
// idle_evolution.go IdleEvolutionScheduler.Run 的"启动即扫一次"一致，
// 避免进程刚起来后的第一个 scanInterval 窗口内让已知的悬挂会话空等。
func (r *AwaitingHandoffReconciler) Run(ctx context.Context) {
	if err := r.Reconcile(ctx); err != nil {
		slog.WarnContext(ctx, "AwaitingHandoffReconciler: initial reconcile failed", "err", err)
	}

	ticker := time.NewTicker(r.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				slog.WarnContext(ctx, "AwaitingHandoffReconciler: periodic reconcile failed", "err", err)
			}
		}
	}
}

// Reconcile 扫描并恢复所有处于 await_agent 状态的会话。非阻塞：每个候选
// 会话的实际等待/归还发生在独立 goroutine 中，本方法只负责派发。
func (r *AwaitingHandoffReconciler) Reconcile(ctx context.Context) error {
	rows, err := r.checkpointRepo.ListByStatus(ctx, "await_agent")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "AwaitingHandoffReconciler: list checkpoints failed", err)
	}
	for _, row := range rows {
		if row.Reason != "handoff_wait" {
			continue
		}
		sessionID := row.TaskID
		childTaskID := row.NodeID

		if _, loaded := r.inFlight.LoadOrStore(sessionID, struct{}{}); loaded {
			slog.InfoContext(ctx, "AwaitingHandoffReconciler: session already being reconciled, skipping this scan",
				"session_id", sessionID)
			continue
		}

		concurrent.SafeGo(ctx, "agent.handoff_reconciler."+sessionID, func(ctx context.Context) {
			defer r.inFlight.Delete(sessionID)
			r.reconcileOne(ctx, sessionID, childTaskID)
		})
	}
	return nil
}

// reconcileOne 恢复单个会话：Acquire → 就位 FSM → （子任务已终态则直接
// 触发／否则重挂 watcher）→ 有界等待会话到达终态 → release。
func (r *AwaitingHandoffReconciler) reconcileOne(ctx context.Context, sessionID, childTaskID string) {
	ctrl, release, err := r.pool.Acquire(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "AwaitingHandoffReconciler: acquire agent failed, skipping",
			"session_id", sessionID, "err", err)
		return
	}
	defer release()

	ag, ok := ctrl.(*Agent)
	if !ok {
		slog.WarnContext(ctx, "AwaitingHandoffReconciler: agent is not of type *Agent", "session_id", sessionID)
		return
	}

	// 就位到崩溃前的 S_AWAIT_AGENT，否则下面投递的 Trigger 会命中
	// Dispatch() 的 "no transition from S_IDLE" 硬错误。
	ag.ResumeAwaitingHandoff(childTaskID)

	drainCtx, cancel := context.WithTimeout(ctx, r.drainTimeout)
	defer cancel()

	// 与正常交互/崩溃恢复路径相同的"先订阅后触发"顺序，消除早期事件丢失竞态。
	stream := ctrl.SubscribeStream(drainCtx)

	task, err := r.handoffPoster.PeekTask(ctx, childTaskID)
	if err != nil {
		slog.WarnContext(ctx, "AwaitingHandoffReconciler: peek task failed",
			"session_id", sessionID, "child_task_id", childTaskID, "err", err)
		return
	}
	if task != nil && (task.Status == types.TaskDone || task.Status == types.TaskFailed) {
		// 子任务已完成，直接可以唤醒 FSM。
		if sendErr := ctrl.SendIntent(types.TriggerAgentHandoffDone); sendErr != nil {
			slog.ErrorContext(ctx, "AwaitingHandoffReconciler: trigger handoff done failed",
				"session_id", sessionID, "err", sendErr)
			return
		}
	} else {
		// 子任务仍在运行，重新挂载 watcher（绑定 Agent 自身生命周期 ctx，
		// 不受本次 drainCtx 超时影响，超时只影响本 goroutine 是否继续持有
		// Pool 槽位等待，不影响 watcher 本身的存活）。
		slog.InfoContext(ctx, "AwaitingHandoffReconciler: re-attaching watcher",
			"session_id", sessionID, "child_task_id", childTaskID)
		ag.watchHandoffCompletion(childTaskID)
	}

	// 阻塞等待会话到达终态（stream 关闭）或超时，超时/出错都记录明确日志，
	// 说明本次 release 是"降级放弃"而非"正常完成"（与 ResumeAwaitingHandoff
	// 文档注释中"消除永久死锁，不是无损续跑"的既定限制一致）。
drainLoop:
	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				// 通道关闭仅意味着 drainCtx 已经结束（SubscribeStream 的注销
				// goroutine 绑定的是 ctx.Done()，不是 FSM 终态本身），不能直接
				// 当作"正常完成"——下面统一查 ctrl.CurrentState() 判定真实结果。
				break drainLoop
			}
			if ev.Type == types.AgentStreamEventError {
				slog.WarnContext(ctx, "AwaitingHandoffReconciler: session ended with error after resume",
					"session_id", sessionID, "detail", ev.Content)
				return
			}
		case <-drainCtx.Done():
			break drainLoop
		}
	}

	switch ctrl.CurrentState() {
	case types.AgentStateComplete, types.AgentStateFailed:
		slog.InfoContext(ctx, "AwaitingHandoffReconciler: session reached terminal state, released",
			"session_id", sessionID, "final_state", ctrl.CurrentState())
	default:
		slog.WarnContext(ctx, "AwaitingHandoffReconciler: timed out waiting for session to reach terminal state, releasing (will abort non-terminal agent per pool.go GD-13-002 invariant)",
			"session_id", sessionID, "timeout", r.drainTimeout, "state_at_timeout", ctrl.CurrentState())
	}
}
