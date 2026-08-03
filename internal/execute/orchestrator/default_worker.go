// default_worker.go：通用兜底 Blackboard 任务消费者。
//
// 背景（2026-07-12 unwired-code-audit 复查）：M8 Orchestrator 的中心化按类型下推
// 机制（RegisterWorker/dispatchPendingTasks，见 orchestrator.go）在生产环境从未
// 被激活——boot_agent.go 从未调用 RegisterWorker，workers map 恒为空；即便非空，
// 唯一注册 Agent（agent-0, Skills=["general"]）与真实生产任务类型（"agent_query"/
// "provider_recovered"/curriculum 动态技能名）在 FindBestAgent 的能力子集校验下
// 永远不匹配。三条真实生产路径因此静默进入黑洞：
//   - POST /v1/agent/query（server_handlers.go handleAgentQuery）
//   - ProviderRecoveryHandler 的 PostTask 兜底分支（agent/recovery.go）
//   - AutoCurriculumGenerator 自动出题（learning/curriculum/curriculum.go）
//
// 任务发布后无人认领，30 分钟饥饿后被 Reaper 强制 failed，调用方轮询
// GET /v1/agent/tasks/{taskID} 永远只能看到 pending。
//
// 修复沿用团队同日已验证并落地的模式（workflowadmin.RunStepWorkerLoop）：不依赖
// 失效的中心化下推，改为自订阅 task_posted 事件 + CAS 认领，只是本 Worker 不针对
// 单一专用能力类型，而是作为"专用 Worker 未认领"的通用兜底——把 Intent 原样当作
// 一次 headless 查询交给 AgentPool.AcquireHeadless（Agent Kernel 完整 FSM/DAG/
// 安全 Gate 能力），执行结果写回 CompleteTask/FailTask。
package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// defaultTaskWorkerAgentID 是本 Worker 在 Blackboard claimed_by 列中的固定标识。
const defaultTaskWorkerAgentID = "default_task_worker"

// defaultTaskExecTimeout 是单个兜底任务的执行上限（headless 推理可能触发多轮
// 工具调用，参照 workflowadmin.executeStepTask 的 30 分钟量级，此处任务粒度更
// 轻量，取 10 分钟）。
const defaultTaskExecTimeout = 10 * time.Minute

// DefaultTaskWorker 自订阅 Blackboard task_posted 事件，认领所有未被专用 Worker
// （通过 excludeTypes 声明的能力类型，如 "workflow_step"）声明的任务，将
// TaskEntry.Intent 原样作为 headless 查询文本执行。
type DefaultTaskWorker struct {
	bb              protocol.Blackboard
	pool            protocol.AgentPool
	excludeTypes    map[string]struct{}
	excludePrefixes []string
}

// NewDefaultTaskWorker 构造通用兜底 Worker。excludeTypes 列出已有专用自订阅
// Worker 处理的能力类型/前缀（例如 workflowadmin 包的 "workflow_step"、
// ADR-0084 的 "agent_handoff:mcp:"），避免与其竞争 CAS 认领——专用 Worker 对
// 这些类型的 Intent 有结构化解析预期（如 JSON envelope、mcp: 委派语义），若被
// 本 Worker 误认领会把结构化数据当纯文本传给 LLM。
//
// 精确类型（如 "debate"）与前缀（如 "agent_handoff:mcp:"）统一按前缀匹配处理
// ——精确类型本身也满足"是自己的前缀"，不需要区分两套判定逻辑（ADR-0084）。
func NewDefaultTaskWorker(bb protocol.Blackboard, pool protocol.AgentPool, excludeTypes ...string) *DefaultTaskWorker {
	ex := make(map[string]struct{}, len(excludeTypes))
	prefixes := make([]string, 0, len(excludeTypes))
	for _, t := range excludeTypes {
		ex[t] = struct{}{}
		prefixes = append(prefixes, t)
	}
	return &DefaultTaskWorker{bb: bb, pool: pool, excludeTypes: ex, excludePrefixes: prefixes}
}

// isExcluded 判断 taskType 是否属于专用 Worker 声明的能力类型/前缀。
func (w *DefaultTaskWorker) isExcluded(taskType string) bool {
	if _, ok := w.excludeTypes[taskType]; ok {
		return true
	}
	for _, p := range w.excludePrefixes {
		if strings.HasPrefix(taskType, p) {
			return true
		}
	}
	return false
}

// RunLoop 是本 Worker 的主守护协程，应在 boot 阶段以 concurrent.SafeGo 或
// Supervisor Worker 形式启动为长驻 goroutine。ctx 取消时返回 ctx.Err()。
func (w *DefaultTaskWorker) RunLoop(ctx context.Context) error {
	if w.bb == nil || w.pool == nil {
		return apperr.New(apperr.CodeInternal, "default task worker: blackboard/agent pool 未注入")
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := w.bb.Subscribe(subCtx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "default task worker: subscribe failed", err)
	}
	slog.Info("default task worker: started listening on blackboard")

	for {
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "default task worker: context canceled", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "default task worker: blackboard event channel closed")
			}
			if ev.Type != "task_posted" {
				continue
			}
			taskID := ev.TaskID
			concurrent.SafeGo(ctx, "orchestrator.default_task_worker.claim", func(ctx context.Context) {
				w.tryClaimAndExecute(ctx, taskID)
			})
		}
	}
}

// tryClaimAndExecute 校验任务不属于专用能力类型后 CAS 认领并执行；非本 Worker
// 应处理的类型/已被抢占/未处于 Pending 状态均静默跳过（与
// orchestrator.Worker.tryClaimAndExecute、workflowadmin.tryClaimAndExecuteStep
// 的"被抢占则无视"语义一致）。
func (w *DefaultTaskWorker) tryClaimAndExecute(ctx context.Context, taskID string) {
	snap, err := w.bb.PeekTask(ctx, taskID)
	if err != nil || snap == nil {
		return
	}
	if w.isExcluded(snap.Type) {
		return // 属于专用 Worker 的能力类型/前缀，让其处理
	}
	if snap.Status != types.TaskPending {
		return
	}

	claimed, err := w.bb.ClaimTask(ctx, taskID, defaultTaskWorkerAgentID)
	if err != nil || !claimed {
		return // 被别的 Worker 抢先认领，无视
	}

	prompt := string(snap.Intent)
	if prompt == "" {
		w.failTask(taskID, "empty task intent, nothing to execute")
		return
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), defaultTaskExecTimeout)
	defer cancel()

	// A16: 恢复跨 goroutine trace 连贯性
	bgCtx = trace.ContextWithRemoteSpan(bgCtx, snap.TraceID, snap.SpanID)

	// [A-03 Step5 豁免] 直接调用 AgentPool.AcquireHeadless，不经
	// internal/gateway/session.Orchestrator.RunTurn(Headless:true)：Blackboard
	// DAG 任务执行无 sessionID/多轮历史/消息持久化语义，不是"会话轮次"，是
	// 一次性任务执行。SystemPromptGuard 防泄露保护仍然生效——由
	// AcquireHeadless（internal/agent/pool.go）自身在返回前统一扫描，覆盖
	// 包括本调用在内的全部直接/间接调用方（见 session/guard.go 顶部注释的
	// 决策记录）。
	// ADR-0084：透传 SpawnDepth，使 transfer_to_agent 委派链深度校验对本兜底
	// 路径执行的委派任务同样生效（此前恒为 0，见 agent_handoff.go）。
	// GD-14-001（2026-08-02 补齐）：同时透传 Namespace，使本地角色委派
	// （agent_handoff:<role>，非 mcp: 前缀）与发起方共享记忆检索范围
	// （此前恒为空串，AcquireHeadless 从未调用 SetMemoryNamespace，见
	// 99-new-findings.md 阶段05 P-03 续 发现 / ADR-0084"已知限制"）。
	// GD-13-001：注册事件回调，透传子 Agent 事件到 Blackboard
	eventCb := types.WithEventCallback(func(ev types.AgentStreamEvent) {
		// 附加嵌套标记及当前委派的角色
		ev.IsNested = true
		ev.ParentTaskID = taskID
		ev.ChildAgentRole = strings.TrimPrefix(snap.Type, "agent_handoff:")
		w.bb.PublishTaskEvent(taskID, ev)
	})

	res, err := w.pool.AcquireHeadless(bgCtx, types.Intent{Query: prompt},
		types.WithSpawnDepth(snap.SpawnDepth), types.WithNamespace(snap.Namespace), eventCb)
	if err != nil {
		slog.Warn("default task worker: headless execution failed", "task_id", taskID, "type", snap.Type, "err", err)
		w.failTask(taskID, err.Error())
		return
	}

	if err := w.bb.CompleteTask(context.Background(), taskID, defaultTaskWorkerAgentID, []byte(res.Output)); err != nil {
		slog.Warn("default task worker: CompleteTask failed", "task_id", taskID, "err", err)
	}
}

func (w *DefaultTaskWorker) failTask(taskID, msg string) {
	if err := w.bb.FailTask(context.Background(), taskID, defaultTaskWorkerAgentID, []byte(msg)); err != nil {
		slog.Warn("default task worker: FailTask failed", "task_id", taskID, "err", err)
	}
}
