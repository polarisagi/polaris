// debate_worker.go：GD-6 PatternDebate 专用 Blackboard 任务消费者。
//
// 背景（ADR-0062/白名单行18-19）：DebateExecutor.Execute / saveCheckpoint 实现完整、
// 有单元测试，但缺少调度驱动——无任何组件在子任务完成后重调用 Execute，导致辩论
// 在首次挂起后停滞。本 Worker 补齐该驱动环节。
//
// 任务约定：
//   - task.Type == "debate"（专用类型，DefaultTaskWorker 排除它）
//   - task.Intent 为 JSON 序列化的 DebateJobIntent（见下方类型定义）
//
// 执行语义（与 GD-1 watchHandoffCompletion 同类模式）：
//  1. 监听 task_posted 事件，认领 type=="debate" 的任务。
//  2. 调用 DebateExecutor.Execute；若返回 "suspend" 错误，则监听 task_completed/
//     task_failed 事件，等待飞行中的子任务终态后重调用 Execute（实现断点续跑）。
//  3. Execute 返回 verdict 时 CompleteTask；返回非 suspend 真实错误时 FailTask。
package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// debateWorkerAgentID 是本 Worker 在 Blackboard claimed_by 列中的固定标识。
const debateWorkerAgentID = "debate_worker"

// debateTaskType 是本 Worker 专用的任务类型前缀。DefaultTaskWorker 应将其加入
// excludeTypes 列表，避免争抢认领导致 Intent JSON 被当作纯文本传给 LLM。
const DebateTaskType = "debate"

// DebateJobIntent 是 DebateWorker 从 task.Intent JSON 中解析的作业描述。
// 发起方通过 json.Marshal(DebateJobIntent{...}) 构造 task.Intent 并 PostTask。
type DebateJobIntent struct {
	Proponent types.TaskEntry `json:"proponent"`  // 正方角色任务描述
	Opponent  types.TaskEntry `json:"opponent"`   // 反方角色任务描述
	Judge     types.TaskEntry `json:"judge"`      // 法官角色任务描述
	MaxRounds int             `json:"max_rounds"` // 最大辩论轮次（0→默认3）
}

// DebateWorker 自订阅 Blackboard task_posted / task_completed / task_failed 事件，
// 认领 type=="debate" 的任务并驱动 DebateExecutor 的多步骤 checkpoint 状态机。
type DebateWorker struct {
	bb protocol.Blackboard
	de *DebateExecutor

	// wakeMu/inFlight/pendingWake 防"丢失唤醒"：子任务可能在父任务 Execute
	// 仍在跑（尚未进入 suspended）时就完成，此时恢复事件看到父任务处于
	// running 只能放弃；记下待唤醒标记，父任务挂起后立即自行补一次恢复。
	wakeMu      sync.Mutex
	inFlight    map[string]bool
	pendingWake map[string]bool
}

// debateSuspendTTL 辩论等待子任务期间的挂起时长上限（写入 expires_at，仅作
// 运维可观测的截止戳；挂起态不受租约 Reaper 回收，由子任务终态事件唤醒）。
const debateSuspendTTL = time.Hour

// NewDebateWorker 构造 DebateWorker。bb 和 de 必须非 nil。
func NewDebateWorker(bb protocol.Blackboard, de *DebateExecutor) *DebateWorker {
	return &DebateWorker{bb: bb, de: de, inFlight: map[string]bool{}, pendingWake: map[string]bool{}}
}

// RunLoop 是本 Worker 的主守护协程，应在 boot 阶段注册到 Supervisor 或以
// concurrent.SafeGo 形式启动为长驻 goroutine。ctx 取消时返回。
func (w *DebateWorker) RunLoop(ctx context.Context) error {
	if w.bb == nil || w.de == nil {
		return apperr.New(apperr.CodeInternal, "debate worker: blackboard/debateExec 未注入")
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := w.bb.Subscribe(subCtx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "debate worker: subscribe failed", err)
	}
	slog.Info("debate worker: started listening on blackboard")

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "debate worker: event channel closed")
			}
			switch ev.Type {
			case "task_posted":
				// 新辩论任务发布，尝试认领并进入状态机
				concurrent.SafeGo(ctx, "orchestrator.debate_worker.claim", func(ctx context.Context) {
					w.tryClaimAndResume(ctx, ev.TaskID)
				})
			case "task_completed", "task_failed":
				// 子任务终态事件——驱动等待中的辩论任务恢复执行。
				// 策略：扫描所有 in-flight 的 debate 任务并重调 Execute。
				// 实际上，saveCheckpoint 里持久化了父任务 ID；此处通过前缀
				// 约定（"debate-<parentID>-<round>-<speaker>"，见 pattern_debate.go:144）
				// 逆推父任务 ID，避免额外的"反向索引"表。
				parentID := debateParentFromChildID(ev.TaskID)
				if parentID == "" {
					continue
				}
				concurrent.SafeGo(ctx, "orchestrator.debate_worker.resume", func(ctx context.Context) {
					w.tryClaimAndResume(ctx, parentID)
				})
			}
		}
	}
}

// debateParentFromChildID 从子任务 ID（格式 "debate-<parentID>-<round>-<speaker>"）
// 逆推父任务 ID。ID 不符合 debate 前缀格式时返回空串（不处理）。
// 约定来自 pattern_debate.go:144：
//
//	target.ID = fmt.Sprintf("debate-%s-%d-%s", parentTaskID, state.Round, state.CurrentSpeaker)
func debateParentFromChildID(childID string) string {
	// 格式：debate-<parentID>-<digit>-<speaker>
	// parentID 本身可能含"-"，因此取第一个和最后两个"-"之间的部分。
	const prefix = "debate-"
	if !strings.HasPrefix(childID, prefix) {
		return ""
	}
	rest := childID[len(prefix):] // "<parentID>-<round>-<speaker>"
	// 最后两段为 "<round>-<speaker>"，round 是纯数字，speaker 是 alpha
	// 找最后一个"-"（speaker），再找倒数第二个"-"（round）
	lastDash := strings.LastIndex(rest, "-")
	if lastDash < 0 {
		return ""
	}
	withoutSpeaker := rest[:lastDash] // "<parentID>-<round>"
	secondLastDash := strings.LastIndex(withoutSpeaker, "-")
	if secondLastDash < 0 {
		return ""
	}
	return withoutSpeaker[:secondLastDash] // "<parentID>"
}

// tryClaimAndResume 驱动 debate 任务的 DebateExecutor.Execute 状态机直到挂起或终态。
//
// GR-6.2-002：两条入口的所有权获取方式不同——
//   - 新任务（pending）：ClaimTask → StartExecution（claimed→running）；
//   - 子任务终态唤醒（suspended，claimed_by=本 Worker）：ResumeFromHITL
//     （suspended→running，SQL 内校验 claimed_by）。
//
// 原实现两条路径都要求 pending 并重复 ClaimTask，首轮挂起后父任务处于
// claimed，唤醒永远被拦下，辩论停滞到租约被 Reaper 回收为止。
// Execute 返回 "suspend" 时调用 SuspendForHITL 进入挂起态：挂起期间不持有
// 租约，也不会被租约 Reaper 当成超时任务回收。
func (w *DebateWorker) tryClaimAndResume(ctx context.Context, taskID string) {
	snap, err := w.bb.PeekTask(ctx, taskID)
	if err != nil || snap == nil {
		return
	}
	if snap.Type != DebateTaskType {
		return // 不是 debate 类型任务
	}
	if !w.resumable(taskID, snap.Status) {
		return
	}
	if !waitReplayDone(ctx) {
		return
	}

	var job DebateJobIntent
	if err := json.Unmarshal(snap.Intent, &job); err != nil {
		slog.Warn("debate worker: invalid intent JSON, failing task", "task_id", taskID, "err", err)
		// L2（脑裂关键）：FailTask 失败意味着任务既没成功也没被标失败，只能靠
		// Reaper 的 expires_at 超时兜底捡回；必须 Error 级（非 Warn）+ counter，
		// 因为这是"双写不一致"的前兆，运维需要立刻可见而非淹没在普通 Warn 里。
		if ftErr := w.bb.FailTask(ctx, taskID, debateWorkerAgentID, []byte("invalid debate intent: "+err.Error())); ftErr != nil {
			slog.ErrorContext(ctx, "debate worker: FailTask failed after invalid intent, task stuck until reaper timeout", "task_id", taskID, "agent_id", debateWorkerAgentID, "err", ftErr)
			metrics.GlobalBlackboardFailTaskErrorsTotal.Add(1)
		}
		return
	}
	if job.MaxRounds <= 0 {
		job.MaxRounds = 3 // 默认轮次
	}

	if !w.acquire(ctx, taskID, snap.Status) {
		return // 被其他协程抢先认领/恢复，无视
	}
	w.wakeMu.Lock()
	w.inFlight[taskID] = true
	w.wakeMu.Unlock()
	defer func() {
		w.wakeMu.Lock()
		delete(w.inFlight, taskID)
		delete(w.pendingWake, taskID)
		w.wakeMu.Unlock()
	}()

	execCtx, release := HoldLease(ctx, w.bb, taskID, debateWorkerAgentID)
	verdict, err := w.de.Execute(execCtx, taskID, job.Proponent, job.Opponent, job.Judge, job.MaxRounds)
	release()
	if err != nil {
		errMsg := err.Error()
		if errMsg == "suspend" {
			if w.suspendAndMaybeWake(ctx, taskID) {
				w.tryClaimAndResume(ctx, taskID)
			}
			return
		}
		// 真实错误，标记失败
		slog.Warn("debate worker: Execute failed", "task_id", taskID, "err", err)
		// L2（脑裂关键）：同上，FailTask 失败必须 Error 级 + counter。
		if ftErr := w.bb.FailTask(ctx, taskID, debateWorkerAgentID, []byte(errMsg)); ftErr != nil {
			slog.ErrorContext(ctx, "debate worker: FailTask failed after Execute error, task stuck until reaper timeout", "task_id", taskID, "agent_id", debateWorkerAgentID, "err", ftErr)
			metrics.GlobalBlackboardFailTaskErrorsTotal.Add(1)
		}
		return
	}

	// Execute 返回 verdict，辩论完成
	if err := w.bb.CompleteTask(ctx, taskID, debateWorkerAgentID, verdict); err != nil {
		slog.Warn("debate worker: CompleteTask failed", "task_id", taskID, "err", err)
	}
}

// acquire 按任务当前状态获取执行所有权：pending 走认领+开始执行，suspended
// 走恢复（SQL 内校验 claimed_by 为本 Worker）。
func (w *DebateWorker) acquire(ctx context.Context, taskID string, status types.TaskStatus) bool {
	if status == types.TaskSuspended {
		return w.bb.ResumeFromHITL(ctx, taskID, debateWorkerAgentID, true) == nil
	}
	claimed, err := w.bb.ClaimTask(ctx, taskID, debateWorkerAgentID)
	if err != nil || !claimed {
		return false
	}
	if err := w.bb.StartExecution(ctx, taskID, debateWorkerAgentID); err != nil {
		slog.Warn("debate worker: StartExecution failed after claim", "task_id", taskID, "err", err)
		return false
	}
	return true
}

// suspendAndMaybeWake 挂起父任务等待子任务终态；若 Execute 期间已有子任务
// 完成（pendingWake），挂起后立即补一次恢复，避免丢失唤醒导致永久挂起。
// 返回 true 表示调用方应立即再恢复一次。
func (w *DebateWorker) suspendAndMaybeWake(ctx context.Context, taskID string) bool {
	deadline := time.Now().Add(debateSuspendTTL).Unix()
	if err := w.bb.SuspendForHITL(ctx, taskID, debateWorkerAgentID, deadline); err != nil {
		slog.Warn("debate worker: suspend failed, task left for lease reaper", "task_id", taskID, "err", err)
		return false
	}
	slog.Debug("debate worker: task suspended, waiting for sub-task completion", "task_id", taskID)
	w.wakeMu.Lock()
	wake := w.pendingWake[taskID]
	delete(w.pendingWake, taskID)
	delete(w.inFlight, taskID)
	w.wakeMu.Unlock()
	return wake
}

// resumable 判断当前状态是否可由本次调用驱动：pending/suspended 可以；
// claimed/executing 说明父任务 Execute 仍在跑，记下待唤醒由其挂起后自行补一次恢复；
// 终态不干预。
func (w *DebateWorker) resumable(taskID string, status types.TaskStatus) bool {
	switch status {
	case types.TaskPending, types.TaskSuspended:
		return true
	case types.TaskClaimed, types.TaskExecuting:
		w.wakeMu.Lock()
		if w.inFlight[taskID] {
			w.pendingWake[taskID] = true
		}
		w.wakeMu.Unlock()
	}
	return false
}
