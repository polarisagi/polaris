package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// mcpA2ATargetPrefix 标记 target_agent_role 指向跨框架外部 Agent（ADR-0084）。
// 命中该前缀时：(1) 强制返回结果 TaintHigh（A1）；(2) 投递前对 context_summary
// 做 PII 脱敏（A6）。实际路由/超时/深度校验由 internal/execute/orchestrator
// 的 MCPA2AWorker 承担，本文件不关心 mcp: 目标的内部寻址语义。
const mcpA2ATargetPrefix = "mcp:"

// D5（GD-14-004）→ GD-1（local_playground/upgrade/01-架构设计变更规范.md）→
// GD-13-007/GD-13-009（阶段04 A-01/A-02）：工具化 Multi-Agent Handoff，
// 异步挂起 + 事件驱动唤醒 + 崩溃无损续跑版本。
//
// 历史背景：原实现（D5 首版）为同步阻塞——`transfer_to_agent` 工具调用内部
// 轮询等待子任务终态，独占一个 DAG 执行槽位直至委派完成。GD-1 复核后改为
// 异步挂起：FSM 新增 S_AWAIT_AGENT 状态（见 docs/arch/spec/state.yaml），
// `executeTransferToAgent` 首次调用时投递子任务后立即返回
// `ToolResult{Suspended:true}` 并推进 TriggerAwaitAgent，不再阻塞当前
// goroutine。真正的恢复由 `watchHandoffCompletion` 启动的后台 watcher
// 完成（见本文件下方，GD-13-007 起改为 Blackboard 事件订阅驱动，轮询降级
// 为兜底），完成后投递 TriggerAgentHandoffDone 使 FSM 回到 S_EXECUTE，
// `runExecuteDAG` 重新进入本函数时，`a.sCtx.HandoffTaskID` 非空触发
// "恢复检查"分支，直接返回子任务结果，不重新投递任务。
//
// 崩溃安全边界（GD-13-009 起，见 agent_handoff_resume.go）：进程重启场景由
// AwaitingHandoffReconciler 扫描 task_checkpoints（reason='handoff_wait'）
// 驱动恢复。`ResumeAwaitingHandoff` 反序列化落盘的 HandoffResumeContext
// 快照（含 DAGModel/ExecuteResult/CompletedNodeIDs/GlobalTaintLevel），
// 强制重跑 S_VALIDATE 四层校验后回填执行期上下文，使新建 Agent 实例能够
// 从委派节点下游继续执行，而非仅仅"消除死锁后直接终态"。快照缺失或校验
// 失败时仍会安全降级为旧行为（ResumeAwaitingHandoff 返回 restored=false），
// 不会因反序列化输入异常而 panic 或误放行未经校验的 DAG。

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
				TaintLevel: resolveHandoffResultTaint(targetRole, taintLevel),
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

	if strings.HasPrefix(targetRole, mcpA2ATargetPrefix) {
		contextSummary = a.redactForMCPEgress(ctx, targetRole, contextSummary)
	}

	childID := fmt.Sprintf("handoff-%s-%s", a.ID, uuid.NewString())
	entry := &types.TaskEntry{
		ID:          childID,
		Type:        "agent_handoff:" + targetRole,
		Priority:    5,
		Intent:      []byte(contextSummary),
		IntentTaint: taintLevel,
		Namespace:   namespace,
		// SpawnDepth（ADR-0084）：复用既有 inv_M8_06 委派链深度校验
		// （SQLiteBlackboard.PostTask resolveMaxDepth），此前恒为 0 从未生效。
		SpawnDepth: a.sCtx.SpawnDepth + 1,
	}

	if err := a.handoffPoster.PostTask(ctx, entry); err != nil {
		// [ADR-0084] SpawnDepth 校验现实际可触发 CodeForbidden（此前 SpawnDepth
		// 恒为 0 从未命中）；用 apperr.CodeOf 保留分类，避免固定 CodeInternal
		// 把深度超限误报成内部错误。
		return nil, apperr.Wrap(apperr.CodeOf(err), "transfer_to_agent: post task", err)
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

// resolveHandoffResultTaint 计算委派恢复分支返回给 DAG 执行器的 TaintLevel。
// mcp: 前缀目标（ADR-0084 A1）强制 TaintHigh（only-up，即使发起方自身会话
// 污点更低也不得降级）；本地角色委派保持既有行为不变。
func resolveHandoffResultTaint(targetRole string, taintLevel types.TaintLevel) types.TaintLevel {
	if strings.HasPrefix(targetRole, mcpA2ATargetPrefix) {
		return types.PropagateTaint(taintLevel, types.TaintHigh)
	}
	return taintLevel
}

// redactForMCPEgress 对即将离开本地信任边界的 context_summary 做 PII 静态
// 脱敏（ADR-0084 A6）：不可逆替换，非 RedactWithMode 的可逆令牌化——外部方
// 不应持有能被复原的映射。PIIDetector 未注入或脱敏失败时记录 Warn 后放行，
// 与既有 headless 路径的 PII 降级策略一致（Tier-0 无 Presidio 场景不阻断
// 主流程）。
func (a *Agent) redactForMCPEgress(ctx context.Context, targetRole, contextSummary string) string {
	if a.Security.PIIDetector == nil {
		slog.WarnContext(ctx, "transfer_to_agent: PIIDetector not injected, mcp: egress skips desensitization",
			"target_role", targetRole)
		return contextSummary
	}
	cleaned, _, redactErr := a.Security.PIIDetector.Redact(ctx, contextSummary)
	if redactErr != nil {
		slog.WarnContext(ctx, "transfer_to_agent: PII redact failed on mcp: egress, sending original text",
			"target_role", targetRole, "err", redactErr)
		return contextSummary
	}
	return cleaned
}

// handoffWatchPollInterval 委派完成轮询间隔，仅用于 Subscribe 不可用/失活时
// 的降级路径（阶段04 A-01 前是唯一机制，现在是兜底）。
const handoffWatchPollInterval = 1 * time.Second

// handoffWatchSafetyInterval 事件订阅之外的兜底巡检间隔（阶段04 A-01，
// GD-13-007）。事件驱动是主路径；本兜底只覆盖"订阅通道因实现方内部异常
// 静默失活"这一残余风险，间隔取分钟级即可，不再构成轮询版本的 SQLite 读压。
const handoffWatchSafetyInterval = 2 * time.Minute

// watchHandoffCompletion 启动一个绑定 a.ctx（Agent 生命周期）的后台
// goroutine，订阅 Blackboard 事件流等待委派子任务 childTaskID 到达终态；
// 到达终态后异步投递 TriggerAgentHandoffDone 唤醒 FSM（S_AWAIT_AGENT →
// S_EXECUTE）。
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
//
// [GD-13-007 修复] 原实现以 1s ticker 轮询 PeekTask，在 N 个 Agent 并发委派
// 时对 SQLite 单写者连接池产生 N QPS 的无谓读压。改为订阅
// SQLiteBlackboard 已有的事件广播（`bb.Subscribe`，与 debate_worker.go /
// default_worker.go / pattern_dag.go / pattern_state_graph.go 已确立的
// idiom 一致），事件到达即唤醒，分钟级 safety ticker 只作静默失活兜底。
//
// 订阅者生命周期（防泄漏）：Subscribe 用**本函数派生的** watchCtx（而非
// 直接透传 a.ctx）——若直接传 a.ctx，本函数返回后订阅通道仍会挂在
// SQLiteBlackboard.subscribers 里（无上限 slice），直到整个 Agent 生命周期
// 结束才会因 a.ctx.Done() 被注销；而一个 Agent 在其存活期内可能发起多次
// transfer_to_agent（每次都会调用本函数一次），若不逐次收敛就会随委派次数
// 线性堆积失效订阅者。watchCtx 在本函数任一返回路径（含 defer）都会被
// cancel，确保订阅"即用即还"，与调用次数无关。
func (a *Agent) watchHandoffCompletion(childTaskID string) {
	if a.handoffPoster == nil || childTaskID == "" {
		return
	}
	concurrent.SafeGo(a.ctx, "agent.handoff_watcher", func(parentCtx context.Context) {
		watchCtx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		events, err := a.handoffPoster.Subscribe(watchCtx)
		if err != nil {
			// 订阅失败时不能静默放弃（会导致父任务永久挂在 S_AWAIT_AGENT）：
			// 退回纯轮询兜底，并显式告警说明已降级。
			slog.WarnContext(watchCtx, "agent: handoff subscribe failed, falling back to polling",
				"task_id", childTaskID, "err", err)
			a.pollHandoffCompletion(watchCtx, childTaskID, handoffWatchPollInterval)
			return
		}

		// 订阅就绪后立刻补一次 Peek：覆盖"投递委派任务到订阅生效之间子任务
		// 已终态"的丢事件窗口（与 reconciler_handoff.go 同一 idiom）。
		if a.peekAndWake(watchCtx, childTaskID) {
			return
		}

		// GD-13-001：订阅子 Agent 事件流，透传至父 Agent 的 Stream Channel
		childEvents, cancelSub := a.handoffPoster.SubscribeTaskEvents(childTaskID)
		defer cancelSub()

		a.loopHandoffEvents(watchCtx, childTaskID, events, childEvents)
	})
}

func (a *Agent) loopHandoffEvents(ctx context.Context, childTaskID string, events <-chan types.BlackboardEvent, childEvents <-chan types.AgentStreamEvent) {
	safety := time.NewTicker(handoffWatchSafetyInterval)
	defer safety.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case childEv, ok := <-childEvents:
			if !ok {
				continue
			}
			// 透传至父 Agent 的 Stream Channel，原样转发嵌套事件
			a.publishStreamEvent(childEv)
		case ev, ok := <-events:
			if !ok {
				// 订阅通道被实现方关闭（非 ctx 取消）：降级为轮询，不静默退出。
				slog.WarnContext(ctx, "agent: handoff event stream closed unexpectedly, falling back to polling",
					"task_id", childTaskID)
				a.pollHandoffCompletion(ctx, childTaskID, handoffWatchPollInterval)
				return
			}
			if ev.TaskID != childTaskID {
				continue
			}
			if ev.Type == "task_completed" || ev.Type == "task_failed" {
				metrics.RecordAgentHandoffWake(ctx, "event")
				a.asyncIntent(types.TriggerAgentHandoffDone)
				return
			}
		case <-safety.C:
			if a.peekAndWake(ctx, childTaskID) {
				return
			}
		}
	}
}

// peekAndWake 查询子任务快照，已终态则记录 metrics.RecordAgentHandoffWake
// "peek"、投递唤醒 Trigger 并返回 true。
func (a *Agent) peekAndWake(ctx context.Context, childTaskID string) bool {
	snap, err := a.handoffPoster.PeekTask(ctx, childTaskID)
	if err != nil {
		slog.WarnContext(ctx, "agent: handoff peek failed", "task_id", childTaskID, "err", err)
		return false
	}
	if snap == nil {
		return false
	}
	if snap.Status == types.TaskDone || snap.Status == types.TaskFailed {
		metrics.RecordAgentHandoffWake(ctx, "peek")
		a.asyncIntent(types.TriggerAgentHandoffDone)
		return true
	}
	return false
}

// pollHandoffCompletion 原轮询实现，仅作为 Subscribe 不可用/中途失活时的
// 降级路径（阶段04 A-01）。
func (a *Agent) pollHandoffCompletion(ctx context.Context, childTaskID string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := a.handoffPoster.PeekTask(ctx, childTaskID)
			if err != nil {
				slog.WarnContext(ctx, "agent: handoff poll fallback peek failed", "task_id", childTaskID, "err", err)
				continue
			}
			if snap == nil {
				continue
			}
			if snap.Status == types.TaskDone || snap.Status == types.TaskFailed {
				metrics.RecordAgentHandoffWake(ctx, "poll_fallback")
				a.asyncIntent(types.TriggerAgentHandoffDone)
				return
			}
		}
	}
}
