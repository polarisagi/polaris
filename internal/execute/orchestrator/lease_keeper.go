package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// cancelRegistrar 是 SQLiteBlackboard 暴露给 Reaper/CancelTask 的取消函数登记面。
// 未纳入 protocol.Blackboard 主接口：只有真实黑板实现需要它，测试替身无须实现；
// 不支持时 HoldLease 退化为仅续约。
type cancelRegistrar interface {
	RegisterCancelFunc(taskID string, cancel context.CancelFunc)
	UnregisterCancelFunc(taskID string)
}

// HoldLease 在 Worker 执行认领到的任务期间维持租约，返回派生的执行 ctx 与
// 释放函数（必须在执行结束后调用，通常 defer）。
//
// 解决两个接线断裂（GR-6.2-001 / GR-6.2-005）：
//   - RenewLease 此前全仓无生产调用：租约 60s 到期后执行中的任务会被 Reaper
//     判为超时回收并重新派发，同一任务被两个执行体并发跑；
//   - RegisterCancelFunc 此前无生产调用：Reaper 回收 / CancelTask 中止时
//     cancels 表为空，无法让仍在跑的 goroutine 停下，副作用继续产生。
//
// 续约返回 ErrStaleBlackboardLease 表示租约已被回收或转移，此时立即取消执行
// ctx（fencing：失去所有权的执行体不得再产生副作用）；其他错误视为瞬时故障，
// 下一个心跳重试——TTL 是心跳间隔的 4 倍，容忍连续 3 次失败。
func HoldLease(parent context.Context, bb protocol.Blackboard, taskID, agentID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	reg, hasReg := bb.(cancelRegistrar)
	if hasReg {
		reg.RegisterCancelFunc(taskID, cancel)
	}
	stop := make(chan struct{})
	concurrent.SafeGo(ctx, "orchestrator.lease_heartbeat", func(ctx context.Context) {
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
			}
			err := bb.RenewLease(ctx, taskID, agentID)
			if err == nil {
				continue
			}
			if errors.Is(err, ErrStaleBlackboardLease) {
				slog.Warn("orchestrator: lease lost, aborting execution", "task_id", taskID, "agent_id", agentID)
				cancel()
				return
			}
			slog.Warn("orchestrator: lease renew failed, will retry", "task_id", taskID, "err", err)
		}
	})
	return ctx, func() {
		close(stop)
		if hasReg {
			reg.UnregisterCancelFunc(taskID)
		}
		cancel()
	}
}

// replayPollInterval 回放期间 Worker 推迟认领的轮询间隔。
const replayPollInterval = 200 * time.Millisecond

// waitReplayDone 在崩溃恢复回放（protocol.IsReplaying）期间阻塞，回放结束后
// 返回 true；ctx 取消返回 false（GR-6.2-006）。
//
// Worker 认领后的执行都是外部副作用（MCP 远程委派、headless Agent 推理），
// 回放窗口内执行会重复发起远程调用，或让 headless Agent 抢占属于被恢复
// Agent 的回放 LLM 记录。选择"推迟"而不是"跳过"：task_posted 事件只投递
// 一次，跳过即永久丢失派发；每个事件本就在独立 goroutine 中处理，阻塞等待
// 不影响事件循环。
func waitReplayDone(ctx context.Context) bool {
	for protocol.IsReplaying() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(replayPollInterval):
		}
	}
	return true
}
