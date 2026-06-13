package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// AgentKernel 定义了 Worker 期望的 M4 Kernel 接口。
// 为了打破循环依赖，我们在这里定义最小所需接口。
type AgentKernel interface {
	GetID() string
	Run(ctx context.Context) error
	GetState() protocol.AgentState
	SetTaskID(id string)
	SetTaskIntent(intent []byte)
	GetExecuteResult() []byte
	SendIntent(trigger protocol.AgentTrigger)
	// GetTokenUsage 返回本轮 Run 的分项 token 消耗（Gap-A, HE-Rule-1）。
	GetTokenUsage() (tokensIn, tokensOut, cacheRead int)
}

// Worker 负责桥接 M8 Blackboard 和 M4 Agent Kernel。
// 它通过 ListenLoop 监听黑板，通过 CAS 抢占任务，并交由内部的 AgentKernel 执行。
// 在中心化模式下，它也能直接接收 Orchestrator 的任务定向投递。
type Worker struct {
	agentID      string
	blackboard   protocol.Blackboard
	kernel       AgentKernel
	TaskPushChan chan string
}

// NewWorker 创建一个新的 Worker。
func NewWorker(agentID string, bb protocol.Blackboard, kernel AgentKernel) *Worker {
	return &Worker{
		agentID:      agentID,
		blackboard:   bb,
		kernel:       kernel,
		TaskPushChan: make(chan string, 10),
	}
}

// ListenLoop 是 Worker 的主守护协程。
// 它可以被注册到 Supervisor Tree 中，随生命周期一起管理。
func (w *Worker) ListenLoop(ctx context.Context) error {
	// 1. 订阅黑板事件
	events, err := w.blackboard.Subscribe(ctx)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to subscribe to blackboard", err)
	}

	slog.Info("worker: started listening on blackboard", "agent", w.agentID)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case taskID := <-w.TaskPushChan:
			// 接收到中心化下推的任务
			slog.Debug("worker: received pushed task", "agent", w.agentID, "task_id", taskID)
			w.tryClaimAndExecute(ctx, taskID)
		case ev, ok := <-events:
			if !ok {
				return perrors.New(perrors.CodeInternal, "blackboard event channel closed")
			}

			// 我们目前主要关注 task_posted 事件
			if ev.Type == "task_posted" {
				// 在复杂的逻辑中，这里应该调用 bb.ScanPendingTasks 并根据优先级和依赖排序。
				// 当前简化直接尝试认领对应的 taskID。
				w.tryClaimAndExecute(ctx, ev.TaskID)
			}
		}
	}
}

func (w *Worker) tryClaimAndExecute(ctx context.Context, taskID string) {
	// 1. 尝试 CAS 原子认领
	claimed, err := w.blackboard.ClaimTask(ctx, taskID, w.agentID)
	if err != nil {
		slog.Error("worker: claim error", "agent", w.agentID, "task_id", taskID, "err", err)
		return
	}

	if !claimed {
		// 被别人抢走了，无视
		return
	}

	slog.Info("worker: task claimed", "agent", w.agentID, "task_id", taskID)

	// 2. 将任务注入 AgentKernel
	w.kernel.SetTaskID(taskID)
	w.kernel.SetTaskIntent([]byte(fmt.Sprintf("Handle task: %s", taskID)))

	// 为了不阻塞 ListenLoop（防止错过心跳或其他事件），我们新起一个 goroutine 跑 Kernel，
	// 或者直接在当前阻塞，取决于并发度要求。M8 架构建议 Agent 是单线程专注模型。
	// 但这需要启动一个内部看门狗来处理 RenewLease。

	done := make(chan struct{})
	go func() {
		// 发送初始脉冲
		w.kernel.SendIntent(protocol.TriggerIntentReceived)
		// 阻塞运行直到 FSM 结束 (COMPLETE 或 FAILED)
		_ = w.kernel.Run(ctx)
		close(done)
	}()

	// 3. 租约看门狗 (LeaseHeartbeat)
	ticker := time.NewTicker(15 * time.Second) // 15s heartbeat
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 自动续约
			_ = w.blackboard.RenewLease(ctx, taskID, w.agentID)
		case <-done:
			// 执行结束
			finalState := w.kernel.GetState()

			// Gap-A: 写回 token 消耗，与任务状态写入解耦（记账失败不影响任务结果）
			tokensIn, tokensOut, cacheRead := w.kernel.GetTokenUsage()
			if tokensIn > 0 || tokensOut > 0 {
				if err := w.blackboard.UpdateTaskTokens(ctx, taskID, tokensIn, tokensOut, cacheRead, 0); err != nil {
					slog.Warn("worker: UpdateTaskTokens failed", "task_id", taskID, "err", err)
				}
			}

			// 根据结果写回 Blackboard
			if finalState == protocol.AgentStateFailed {
				slog.Error("worker: task failed", "agent", w.agentID, "task_id", taskID, "err", perrors.New(perrors.CodeInternal, "log event"))
				_ = w.blackboard.FailTask(ctx, taskID, w.agentID, []byte("agent kernel execution failed"))
			} else {
				slog.Info("worker: task completed", "agent", w.agentID, "task_id", taskID)
				// 获取执行成果
				res := w.kernel.GetExecuteResult()
				if res == nil {
					res, _ = json.Marshal(map[string]string{"status": "ok"})
				}
				_ = w.blackboard.CompleteTask(ctx, taskID, w.agentID, res)
			}
			return
		}
	}
}
