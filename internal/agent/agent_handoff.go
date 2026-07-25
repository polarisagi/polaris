package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// D5（GD-14-004）：工具化 Multi-Agent Handoff。
//
// 设计背景（gemini-upgrade-prompt.md D5）：ADR-0050 已删除中心化
// Orchestrator/Worker，多 Agent 协同收敛为 Blackboard CAS 自认领模型。本文件
// 在此模型之上提供一层轻量工具封装——LLM 判断需要委派给另一角色时，直接调用
// transfer_to_agent 工具，无需自行编排完整 StateGraph/Blackboard 订阅。
//
// 实现选择（与原设计的已知偏差，见收尾说明）：原设计设想通过复用
// hitl_suspend/resume_from_suspended 转移使当前 Agent 异步挂起、由目标任务
// 完成事件触发恢复。复核发现该转移属于 M8 task_status（Blackboard 任务生命
// 周期）状态机语义，而非 Agent 认知 FSM（internal/agent/fsm）状态；且 Agent
// 侧真正precedented 的异步挂起路径（spawn_planner→S_INTERRUPT→whisperChan）
// 依赖 PlannerPool 专用的耳语回灌机制，与"等待任意委派任务完成"场景耦合过紧，
// 贸然复用风险高于收益。故本实现改为同步阻塞：转移到目标角色的 Blackboard
// 任务由当前工具调用内部轮询等待（复用 csv_fanout.go 已验证的
// "PostTask→轮询 PeekTask 直至终态" 模式），完成后把结果作为 ToolResult 返回，
// Agent 自身 FSM 不做任何新状态转移（无需变更 state.yaml/fsm/state_machine.go）。
// 优点：零 FSM 变更、零新增后台 watcher 子系统，正确性可通过阻塞轮询直接验证；
// 代价：委派耗时期间占用一个 DAG 节点执行槽位（与 spawn_planner 的真异步挂起
// 相比不能立即释放当前 Agent 去做其他事）。若未来需要真正的非阻塞挂起，需专项
// 设计 Agent FSM 新状态 + 恢复触发器，属于独立于本次修复范围的后续工作。

// handoffPollInterval 与 csv_fanout.go waitForTask 保持一致的轮询间隔。
const handoffPollInterval = 500 * time.Millisecond

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

	result, waitErr := waitForHandoffTask(ctx, a.handoffPoster, childID)
	if waitErr != nil {
		// 委派任务失败/超时不视为节点执行错误（不触发 Saga 补偿）——按 ToolResult
		// 失败语义返回，交由上层 LLM 决定重试或改变计划，与 code_act 失败路径的
		// error-vs-failure 区分原则一致；waitErr 内容已写入 Output 供上层查看，
		// 不静默丢弃。
		return &types.ToolResult{ //nolint:nilerr // 意图返回：委派失败是业务结果不是执行错误
			Success: false,
			Output:  []byte(waitErr.Error()),
		}, nil
	}
	return &types.ToolResult{
		Success:    true,
		Output:     []byte(result),
		TaintLevel: taintLevel,
	}, nil
}

// waitForHandoffTask 轮询 Blackboard 等待委派任务达到终态（done/failed），
// 与 internal/execute/orchestrator/csv_fanout.go 的 waitForTask 采用同一模式。
func waitForHandoffTask(ctx context.Context, poster HandoffPoster, taskID string) (string, error) {
	ticker := time.NewTicker(handoffPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", apperr.New(apperr.CodeInternal,
				fmt.Sprintf("transfer_to_agent: timeout waiting for handoff task %s", taskID))
		case <-ticker.C:
			snap, err := poster.PeekTask(ctx, taskID)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeInternal, "transfer_to_agent: peek task", err)
			}
			if snap == nil {
				continue
			}
			switch snap.Status {
			case types.TaskDone:
				return string(snap.Result), nil
			case types.TaskFailed:
				return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("handoff task %s failed", taskID))
			}
		}
	}
}
