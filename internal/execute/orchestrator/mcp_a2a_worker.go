// mcp_a2a_worker.go：ADR-0084 MCP Agent-to-Agent 出站委派专用 Blackboard 任务消费者。
//
// 背景：transfer_to_agent（agent_handoff.go）投递 "agent_handoff:<role>" 任务，
// 现有唯一消费者 DefaultTaskWorker 把 Intent 原样当纯文本 headless 查询执行，
// 完全丢弃 target_agent_role 的语义。本 Worker 只认领 target_agent_role 带
// "mcp:<server>/<agent>" 前缀的任务（Type == "agent_handoff:mcp:<server>/<agent>"），
// 是该前缀约定第一次被赋予真实运行时语义的地方。
//
// 复用（不新建）：Blackboard 自订阅+CAS 认领 idiom 与 DefaultTaskWorker/
// DebateWorker 一致；实际网络调用复用 MCPManager.CallTool（把外部 Agent 当作
// MCP 工具的一个特殊类别，见 ADR-0084"决策"）。
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPA2AHandoffPrefix 是本 Worker 专用认领的任务类型前缀。DefaultTaskWorker
// 必须将其加入 excludeTypes（前缀匹配，见 default_worker.go），避免争抢认领。
const MCPA2AHandoffPrefix = "agent_handoff:mcp:"

// mcpA2ADelegateToolName 目标 MCP Server 必须暴露的委派入口工具名，须与
// internal/extension/mcp.A2ADelegateToolName 保持一致——本包不直接 import
// internal/extension/mcp（consumer-side 接口见下方 MCPToolCaller，HE-3），
// 故在此复制常量值而非引用，两处如需变更必须同步修改。
const mcpA2ADelegateToolName = "a2a_delegate"

// mcpA2AWorkerAgentID 是本 Worker 在 Blackboard claimed_by 列中的固定标识。
const mcpA2AWorkerAgentID = "mcp_a2a_worker"

// MCPToolCaller 是 MCPA2AWorker 所需的最小 MCP 调用能力（HE-3：接口在调用方
// 定义）。internal/execute（服务 L1+L2）不直接 import internal/extension/mcp
// 的具体类型；*mcp.MCPManager 已天然满足本接口（结构子类型，无需适配器）。
type MCPToolCaller interface {
	CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error)
	ResolveServerIDByName(name string) (string, bool)
}

// MCPA2AWorker 自订阅 Blackboard task_posted 事件，认领
// Type 前缀为 MCPA2AHandoffPrefix 的任务，转译为对目标 MCP Server 的
// a2a_delegate 工具调用（ADR-0084）。
type MCPA2AWorker struct {
	bb      protocol.Blackboard
	mcp     MCPToolCaller
	timeout time.Duration
}

// NewMCPA2AWorker 构造 MCPA2AWorker。timeout<=0 时回落到 10 分钟默认值
// （ADR-0084 A4；生产配置见 config.M8OrchestratorThresholds.A2AHandoffTimeoutSeconds）。
func NewMCPA2AWorker(bb protocol.Blackboard, caller MCPToolCaller, timeout time.Duration) *MCPA2AWorker {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &MCPA2AWorker{bb: bb, mcp: caller, timeout: timeout}
}

// RunLoop 是本 Worker 的主守护协程，应在 boot 阶段以 Supervisor Worker 形式
// 启动为长驻 goroutine。ctx 取消时返回。
func (w *MCPA2AWorker) RunLoop(ctx context.Context) error {
	if w.bb == nil || w.mcp == nil {
		return apperr.New(apperr.CodeInternal, "mcp_a2a_worker: blackboard/mcp caller 未注入")
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := w.bb.Subscribe(subCtx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp_a2a_worker: subscribe failed", err)
	}
	slog.Info("mcp a2a worker: started listening on blackboard")

	for {
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "mcp_a2a_worker: context canceled", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "mcp_a2a_worker: blackboard event channel closed")
			}
			if ev.Type != "task_posted" {
				continue
			}
			taskID := ev.TaskID
			concurrent.SafeGo(ctx, "orchestrator.mcp_a2a_worker.claim", func(ctx context.Context) {
				w.tryClaimAndExecute(ctx, taskID)
			})
		}
	}
}

// tryClaimAndExecute 校验任务属于本 Worker 的能力前缀后 CAS 认领并执行；
// 非本 Worker 应处理的类型/已被抢占/未处于 Pending 状态均静默跳过。
func (w *MCPA2AWorker) tryClaimAndExecute(ctx context.Context, taskID string) {
	snap, err := w.bb.PeekTask(ctx, taskID)
	if err != nil || snap == nil {
		return
	}
	if !strings.HasPrefix(snap.Type, MCPA2AHandoffPrefix) {
		return // 不属于本 Worker 的能力前缀，让 DefaultTaskWorker/其它专用 Worker 处理
	}
	if snap.Status != types.TaskPending {
		return
	}

	claimed, err := w.bb.ClaimTask(ctx, taskID, mcpA2AWorkerAgentID)
	if err != nil || !claimed {
		return // 被其他协程抢先认领，无视
	}

	// [ADR-0084 决策9] 执行前二次深度校验：PostTask 时的 resolveMaxDepth 校验
	// 已经拦截绝大多数超限任务，此处是发起外部网络调用前的最后一道闸——防止
	// 配置热更（上限调低）后，此前已通过校验但尚未认领的任务仍被执行放出。
	if snap.SpawnDepth > MaxSpawnDepth {
		w.failTask(ctx, taskID, fmt.Sprintf(
			"mcp_a2a_worker: spawn depth %d exceeds max %d, refusing to dispatch", snap.SpawnDepth, MaxSpawnDepth))
		return
	}

	target := strings.TrimPrefix(snap.Type, MCPA2AHandoffPrefix)
	serverName, agentName, hasSlash := strings.Cut(target, "/")
	if !hasSlash || serverName == "" {
		w.failTask(ctx, taskID, "mcp_a2a_worker: invalid target format, expected mcp:<server>/<agent>")
		return
	}
	if agentName == "" {
		agentName = "default"
	}

	serverID, ok := w.mcp.ResolveServerIDByName(serverName)
	if !ok {
		w.failTask(ctx, taskID, fmt.Sprintf("mcp_a2a_worker: mcp server %q not connected", serverName))
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	// context_summary 在投递前已由 executeTransferToAgent 做 PII 脱敏
	// （ADR-0084 A6），此处原样透传，不重复处理。
	args := map[string]any{
		"target_agent":    agentName,
		"context_summary": string(snap.Intent),
		"namespace":       snap.Namespace,
	}
	raw, err := w.mcp.CallTool(callCtx, serverID, mcpA2ADelegateToolName, args)
	if err != nil {
		w.failTask(ctx, taskID, err.Error())
		return
	}

	if err := w.bb.CompleteTask(ctx, taskID, mcpA2AWorkerAgentID, []byte(raw)); err != nil {
		slog.Warn("mcp a2a worker: CompleteTask failed", "task_id", taskID, "err", err)
	}
}

// failTask 标记任务失败。FailTask 自身失败必须 Error 级 + counter（L2 脑裂
// 关键，与 debate_worker.go 同一约定）：任务既没成功也没被标失败，只能靠
// Reaper 的 expires_at 超时兜底捡回，运维需要立刻可见。
func (w *MCPA2AWorker) failTask(ctx context.Context, taskID, msg string) {
	if err := w.bb.FailTask(ctx, taskID, mcpA2AWorkerAgentID, []byte(msg)); err != nil {
		slog.ErrorContext(ctx, "mcp a2a worker: FailTask failed, task stuck until reaper timeout",
			"task_id", taskID, "agent_id", mcpA2AWorkerAgentID, "err", err)
		metrics.GlobalBlackboardFailTaskErrorsTotal.Add(1)
	}
}
