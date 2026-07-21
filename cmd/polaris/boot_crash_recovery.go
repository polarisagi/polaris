// boot_crash_recovery.go — M04-Agent-Kernel.md §8 崩溃恢复（2026-07-22 接线）。
//
// 背景：docs/arch/M04-Agent-Kernel.md §8 定义了"EventLog 重放 → 重建
// StateContext → 从崩溃点续跑"的崩溃恢复设计，但 protocol.SetReplayMode 此前
// 只有 4 处读侧护栏（agent_execute_dag.go/agent_execute_effect_helpers.go/
// outbox_worker.go/execute/dag/executor_node.go），从未有任何 Setter 调用点
// （ADR-0052 deadcode 审计发现）。本文件补齐驱动逻辑：boot 阶段扫描上一次
// 运行遗留的 in-flight 标记（internal/agent/agent.go markInFlight/
// clearInFlight，Run() 处理期间写入、正常退出时清除），对候选会话尝试
// TrajectoryRecorderImpl 录像回放式恢复。
//
// 安全边界（用户已知情并明确选择"投入构建完整 StateContext 重建"这一较大
// 方案，而非最小化 Reaper 式重试）：
//   - 仅当会话最后一次记录的状态迁移落点是纯 LLM 状态（S_PERCEIVE/S_PLAN/
//     S_REFLECT）或压根没有状态迁移记录时才自动重放恢复。S_VALIDATE/
//     S_EXECUTE/S_REPLAN/S_ROLLBACK 一律跳过不动：agent_execute_dag.go 的
//     2PC 预写日志机制理论上也能保护 S_EXECUTE 重入时的重复副作用，但本次
//     未对该机制做专门审计/测试，不额外依赖它作为自动恢复的安全网——保守
//     跳过，回落到"崩溃恢复功能上线前"的既有行为（不动它，等人工介入），
//     不引入新的重复副作用风险面。
//   - 必须在 HTTP 服务开始对外服务之前串行执行完毕（main.go 调用时机：
//     bootAgent+LoadProvidersFromDB 之后、bootServer 之前）——全局
//     protocol.ReplayMode 标志是进程级而非会话级，此窗口内不存在其他并发
//     会话与其读取冲突（见 internal/protocol/replay.go 注释）。
//   - 每个会话尝试一次后无论成功/跳过/失败都立即清除 in-flight 标记，不做
//     无限重试——与 M8 Reaper Phase1/Phase2"尽力恢复一次，恢复不了就放弃"
//     的既有哲学一致（internal/execute/orchestrator/reaper.go）。
//   - defer protocol.SetReplayMode(false) 是双重保险：即便 executeEffect 的
//     FastPath/PRM 候选等不检查 replay 队列的分支意外接管了首个 LLMFillEffect
//     （导致队列耗尽翻转逻辑未被触发），会话恢复流程结束时也无条件复位全局
//     标志，防止其永久卡在 true 而误伤后续正常流量。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	inFlightKeyPrefix = "inflight:session:"

	// recoverySessionTimeout 单会话恢复回放的上限，防止某一条异常轨迹
	// （如陷入长时间无响应的 Provider 真实调用）无限期阻塞启动流程。
	recoverySessionTimeout = 2 * time.Minute

	// recoveryMarkerClearTimeout 清除单个 in-flight 标记的 KV 写入超时。
	recoveryMarkerClearTimeout = 2 * time.Second
)

// crashRecoveryReDriveStates 允许自动重放恢复的"崩溃前最后已知状态"白名单
// （均为纯 LLM 协处理器状态，不涉及真实外部副作用）。用 fmt.Sprintf 而非
// 硬编码数字字符串：状态迁移事件落盘时是 fmt.Sprintf("%d", t.To)
// （internal/agent/fsm/state_machine.go Dispatch 末尾），与此处保持同一
// 转换方式，enum 顺序调整时两侧自动保持一致，不会静默失配。只读查找表，
// 构造后不再被修改，等价于 internal/agent/pool.go headlessPromptGuard 的
// sync.OnceValue 单例惯例（此处用普通 var 更简单，无需懒加载）。
//
//nolint:gochecknoglobals // 只读查找表，非可变状态
var crashRecoveryReDriveStates = map[string]bool{
	fmt.Sprintf("%d", types.AgentStatePerceive): true,
	fmt.Sprintf("%d", types.AgentStatePlan):     true,
	fmt.Sprintf("%d", types.AgentStateReflect):  true,
	"": true, // 无状态迁移记录：崩溃发生在第一次状态转移完成之前
}

// recoverCrashedSessions 扫描 "inflight:session:" 前缀标记，对每个候选会话
// 尝试崩溃恢复回放。任何单一会话的恢复失败/跳过都只记日志，不中断启动流程。
func recoverCrashedSessions(ctx context.Context, sb *SubstrateBundle, ab *AgentBundle) {
	if sb == nil || sb.Store == nil || ab == nil || ab.AgentPool == nil {
		return
	}

	iter, err := sb.Store.Scan(ctx, []byte(inFlightKeyPrefix))
	if err != nil {
		slog.Warn("polaris: crash recovery scan failed", "err", err)
		return
	}
	var sessionIDs []string
	for iter.Next() {
		sessionIDs = append(sessionIDs, strings.TrimPrefix(string(iter.Key()), inFlightKeyPrefix))
	}
	scanErr := iter.Err()
	_ = iter.Close()
	if scanErr != nil {
		slog.Warn("polaris: crash recovery scan iteration failed", "err", scanErr)
	}
	if len(sessionIDs) == 0 {
		return
	}

	slog.Warn("polaris: crash recovery detected in-flight sessions from previous run, attempting replay recovery",
		"count", len(sessionIDs))

	recorder := harness.NewTrajectoryRecorder(sb.Store)
	for _, sessionID := range sessionIDs {
		recoverOneSession(ctx, sb.Store.DB(), ab.AgentPool, recorder, sessionID)

		clearCtx, cancel := context.WithTimeout(ctx, recoveryMarkerClearTimeout)
		if delErr := sb.Store.Delete(clearCtx, []byte(inFlightKeyPrefix+sessionID)); delErr != nil {
			slog.Warn("polaris: crash recovery failed to clear in-flight marker", "session", sessionID, "err", delErr)
		}
		cancel()
	}
}

// recoverOneSession 对单个候选会话执行一次恢复尝试。依赖收窄为
// db（读取触发消息）+ pool（获取 Agent 并驱动 FSM），而非整个 *SubstrateBundle/
// *AgentBundle——HE-3 消费方仅声明真正用到的能力，也便于单测用轻量 fake 替身。
func recoverOneSession(ctx context.Context, db *sql.DB, pool protocol.AgentPool, recorder *harness.TrajectoryRecorderImpl, sessionID string) {
	trace, err := recorder.Record(ctx, sessionID)
	if err != nil {
		slog.Warn("polaris: crash recovery failed to read trajectory, abandoning session", "session", sessionID, "err", err)
		return
	}

	lastState := ""
	if n := len(trace.StateTrans); n > 0 {
		lastState = trace.StateTrans[n-1].To
	}
	if !crashRecoveryReDriveStates[lastState] {
		slog.Warn("polaris: crash recovery skipping session, last known state unsafe for unattended auto-recovery (mid tool-execution/validate/recovery state)",
			"session", sessionID, "last_state", lastState)
		return
	}

	lastUserMsg, msgErr := lastUserMessage(ctx, db, sessionID)
	if msgErr != nil || lastUserMsg == "" {
		slog.Warn("polaris: crash recovery found no user message to re-drive session, abandoning", "session", sessionID, "err", msgErr)
		return
	}

	agentCtrl, release, err := pool.Acquire(ctx, sessionID)
	if err != nil {
		slog.Warn("polaris: crash recovery failed to acquire agent", "session", sessionID, "err", err)
		return
	}
	defer release()

	if len(trace.LLMCalls) > 0 {
		calls := make([]protocol.ReplayLLMCall, 0, len(trace.LLMCalls))
		for _, c := range trace.LLMCalls {
			calls = append(calls, protocol.ReplayLLMCall{Request: c.Request, Response: c.Response})
		}
		agentCtrl.InjectReplayData(calls)
		protocol.SetReplayMode(true)
		// 双重保险：见文件头注释——无条件复位，防止全局标志永久卡在 true。
		defer protocol.SetReplayMode(false)
	}

	recoverCtx, cancel := context.WithTimeout(ctx, recoverySessionTimeout)
	defer cancel()

	// 与正常交互/headless 路径完全相同的"先订阅后触发"顺序（见
	// internal/gateway/server/chat/sse.go handleAgentStreamFSM /
	// internal/agent/pool.go AcquireHeadless），消除早期事件丢失竞态。
	stream := agentCtrl.SubscribeStream(recoverCtx)
	agentCtrl.SetTaskIntent([]byte(lastUserMsg))
	if sendErr := agentCtrl.SendIntent(types.TriggerIntentReceived); sendErr != nil {
		slog.Warn("polaris: crash recovery failed to send intent", "session", sessionID, "err", sendErr)
		return
	}

	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				slog.Info("polaris: crash recovery completed", "session", sessionID, "replayed_llm_calls", len(trace.LLMCalls))
				return
			}
			if ev.Type == types.AgentStreamEventError {
				slog.Warn("polaris: crash recovery session ended with error", "session", sessionID, "detail", ev.Content)
				return
			}
		case <-recoverCtx.Done():
			slog.Warn("polaris: crash recovery timed out waiting for session to reach terminal state", "session", sessionID)
			return
		}
	}
}

// lastUserMessage 取会话最后一条 role='user' 的消息内容，作为崩溃恢复重新
// 驱动 FSM 的 Intent——与正常交互路径完全相同的 SetTaskIntent+SendIntent
// 组合。消息本身在 FSM 触发之前已同步写入 chat_messages
// （sse.go SaveMessage 调用早于 SetTaskIntent，见该文件第 133/328 行），故
// 崩溃点无论多早，触发消息必然已经落盘。
func lastUserMessage(ctx context.Context, db *sql.DB, sessionID string) (string, error) {
	row := db.QueryRowContext(ctx,
		`SELECT content FROM chat_messages WHERE session_id = ? AND role = 'user' ORDER BY id DESC LIMIT 1`,
		sessionID)
	var content string
	if err := row.Scan(&content); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "lastUserMessage: scan", err)
	}
	return content, nil
}
