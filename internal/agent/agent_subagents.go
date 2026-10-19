package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SubagentController 允许父 Agent 在同节点或经 Blackboard 对子 Agent 进行插话和中止控制（R2-14）。
type SubagentController interface {
	TrySteer(sessionID, text string) bool
	StopSession(sessionID string) bool
}

// InjectSubagentController 注入子智能体控制器（Pool 实现），支持同节点直接插话与中止（R2-14）。
func (a *Agent) InjectSubagentController(ctrl SubagentController) {
	a.subagentCtrl = ctrl
}

func (a *Agent) countRunningSubagents(ctx context.Context) int {
	a.subagentIDsMu.Lock()
	defer a.subagentIDsMu.Unlock()
	if a.handoffPoster == nil || len(a.spawnedSubagentIDs) == 0 {
		return 0
	}
	running := 0
	active := make([]string, 0, len(a.spawnedSubagentIDs))
	for _, id := range a.spawnedSubagentIDs {
		snap, err := a.handoffPoster.PeekTask(ctx, id)
		if err == nil && snap != nil && snap.Status != types.TaskDone && snap.Status != types.TaskFailed {
			running++
			active = append(active, id)
		}
	}
	a.spawnedSubagentIDs = active
	return running
}

func (a *Agent) cascadeCancelSubagents(ctx context.Context, ids []string) {
	for _, id := range ids {
		if a.handoffPoster != nil {
			if fErr := a.handoffPoster.FailTask(ctx, id, a.ID, []byte("cascade cancelled due to post failure of sibling task")); fErr != nil {
				slog.DebugContext(ctx, "agent_subagents: cascade cancel fail task", "task_id", id, "err", fErr)
			}
		}
		a.updateSubagentStatusCancelled(ctx, id)
	}
}

// SpawnAgents 派生一个或多个子智能体并发执行（ADR-0115 WP4）。
func (a *Agent) SpawnAgents(ctx context.Context, args types.SpawnAgentsArgs) ([]string, error) { //nolint:gocyclo
	if a.handoffPoster == nil {
		return nil, apperr.New(apperr.CodeInternal, "spawn_agents: handoffPoster not injected (fail-closed)")
	}
	if p := a.agentProfile(); p != nil && !p.AllowDelegation {
		return nil, apperr.New(apperr.CodeForbidden, "spawn_agents: subagents with AllowDelegation=false cannot spawn agents (security boundary)")
	}
	if len(args.Tasks) == 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "spawn_agents: tasks cannot be empty")
	}

	pol := a.currentSecurityPolicy()

	// 1. 并发上限检查（按当前运行中的子智能体总数 + 本次新增任务数计算，R2-14）
	maxConcurrent := pol.SubagentMaxConcurrent
	runningCount := a.countRunningSubagents(ctx)
	if maxConcurrent > 0 && runningCount+len(args.Tasks) > maxConcurrent {
		return nil, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("spawn_agents: running subagents %d + new tasks %d exceeds concurrency limit %d for profile %s",
				runningCount, len(args.Tasks), maxConcurrent, pol.Profile))
	}

	// 2. 委派链深度检查
	maxDepth := pol.DelegationChainMaxDepth
	if maxDepth > 0 && a.sCtx.SpawnDepth+1 > maxDepth {
		return nil, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("spawn_agents: delegation depth %d exceeds maximum depth %d",
				a.sCtx.SpawnDepth+1, maxDepth))
	}

	parentModel, userLocked := a.GetModelStatus()
	if parentModel == "" {
		parentModel = userLocked
	}
	namespace := a.sCtx.NamespaceID
	if namespace == "" {
		namespace = a.sCtx.SessionID
	}

	childIDs := make([]string, 0, len(args.Tasks))
	var postedIDs []string
	for i, task := range args.Tasks {
		role := strings.TrimSpace(task.Role)
		if role == "" {
			role = "general-purpose"
		}

		// 未指定模型时，继承父智能体当前生效的模型（Codex 语义）
		model := strings.TrimSpace(task.Model)
		if model == "" {
			model = parentModel
		}
		task.Model = model

		// local_only_mode 校验：子智能体模型不得为云端模型
		if model != "" {
			if err := a.validateModelSelection(ctx, model); err != nil {
				a.cascadeCancelSubagents(ctx, postedIDs)
				return nil, apperr.Wrap(apperr.CodeForbidden, fmt.Sprintf("spawn_agents: task[%d] model %q rejected by policy", i, model), err)
			}
		}

		childID := fmt.Sprintf("subagent-%s-%d-%s", a.ID, i, uuid.NewString()[:8])

		payload, err := json.Marshal(task)
		if err != nil {
			a.cascadeCancelSubagents(ctx, postedIDs)
			return nil, apperr.Wrap(apperr.CodeInternal, "spawn_agents: marshal task spec", err)
		}

		entry := &types.TaskEntry{
			ID:           childID,
			Type:         "agent_handoff:" + role,
			Priority:     5,
			Intent:       payload,
			IntentTaint:  a.sCtx.GlobalTaintLevel,
			Namespace:    namespace,
			SpawnDepth:   a.sCtx.SpawnDepth + 1,
			ParentTaskID: a.ID,
		}

		if pErr := a.handoffPoster.PostTask(ctx, entry); pErr != nil {
			a.cascadeCancelSubagents(ctx, postedIDs)
			cancelMsg := fmt.Sprintf("spawn_agents: post task failed on %s", childID)
			if len(postedIDs) > 0 {
				cancelMsg = fmt.Sprintf("spawn_agents: post task failed on %s (sibling tasks cancelled: %s)",
					childID, strings.Join(postedIDs, ", "))
			}
			return nil, apperr.Wrap(apperr.CodeOf(pErr), cancelMsg, pErr)
		}

		postedIDs = append(postedIDs, childID)
		childIDs = append(childIDs, childID)
		a.recordSubagentRun(ctx, childID, role, model, task.Thinking, task.Prompt)
	}

	a.subagentIDsMu.Lock()
	a.spawnedSubagentIDs = append(a.spawnedSubagentIDs, childIDs...)
	a.subagentIDsMu.Unlock()

	// 3. wait 模式处理
	waitMode := strings.ToLower(strings.TrimSpace(args.Wait))
	if waitMode == "none" {
		return childIDs, nil
	}

	// wait=all 场景：将子智能体集合写入上下文并挂起当前 Agent（R3-05）
	a.sCtx.Mu.Lock()
	a.sCtx.HandoffTaskIDs = append(a.sCtx.HandoffTaskIDs, childIDs...)
	if a.sCtx.HandoffTaskID == "" && len(childIDs) > 0 {
		a.sCtx.HandoffTaskID = childIDs[0] // 兼容既有单值检查点
	}
	if callID := getToolCallID(ctx); callID != "" {
		if a.sCtx.LoopHandoffSpawnCallID == nil {
			a.sCtx.LoopHandoffSpawnCallID = make(map[string][]string)
		}
		a.sCtx.LoopHandoffSpawnCallID[callID] = append(a.sCtx.LoopHandoffSpawnCallID[callID], childIDs...)
	}
	inBatch := ctx.Value(ctxInBatchDelegationKey{}) != nil
	a.sCtx.Mu.Unlock()

	if !inBatch {
		a.watchSubagentsCompletion(childIDs)
		a.asyncIntent(types.TriggerAwaitAgent)
	}

	return childIDs, nil
}

// AgentStatus 查询子智能体的运行状态。
func (a *Agent) AgentStatus(ctx context.Context, ids []string) ([]types.SubagentStatusInfo, error) {
	if a.handoffPoster == nil {
		return nil, apperr.New(apperr.CodeInternal, "agent_status: handoffPoster not injected")
	}

	targetIDs := ids
	if len(targetIDs) == 0 {
		a.sCtx.Mu.RLock()
		targetIDs = a.sCtx.HandoffTaskIDs
		a.sCtx.Mu.RUnlock()
	}

	out := make([]types.SubagentStatusInfo, 0, len(targetIDs))
	for _, id := range targetIDs {
		info := types.SubagentStatusInfo{ID: id, Status: "unknown"}
		snap, err := a.handoffPoster.PeekTask(ctx, id)
		if err == nil && snap != nil {
			switch snap.Status {
			case types.TaskPending:
				info.Status = "pending"
			case types.TaskClaimed, types.TaskExecuting:
				info.Status = "running"
			case types.TaskDone:
				info.Status = "done"
			case types.TaskFailed:
				info.Status = "failed"
				info.Error = string(snap.Result)
			}
			info.Role = strings.TrimPrefix(snap.Type, "agent_handoff:")
		}
		out = append(out, info)
	}
	return out, nil
}

// AgentResult 查询指定子智能体的执行产物，超过限制时卸载为 read_tool_ref（T5 原则）。
func (a *Agent) AgentResult(ctx context.Context, id string) (string, error) {
	if a.handoffPoster == nil {
		return "", apperr.New(apperr.CodeInternal, "agent_result: handoffPoster not injected")
	}
	snap, err := a.handoffPoster.PeekTask(ctx, id)
	if err != nil || snap == nil {
		return "", apperr.New(apperr.CodeNotFound, fmt.Sprintf("agent_result: task %s not found", id))
	}

	if snap.Status != types.TaskDone && snap.Status != types.TaskFailed {
		return fmt.Sprintf("子智能体 %s 处于 %v 状态，尚未完成", id, snap.Status), nil
	}

	resStr := string(snap.Result)
	return a.truncateAndOffloadResult(ctx, id, resStr, 4000), nil
}

// AgentMessage 向指定子智能体发送插话消息（ADR-0115 WP4 / WP2 steer 机制，R3-04）。
func (a *Agent) AgentMessage(ctx context.Context, id, text string) (string, error) {
	if a.handoffPoster == nil {
		return "", apperr.New(apperr.CodeInternal, "agent_message: handoffPoster not injected")
	}
	snap, err := a.handoffPoster.PeekTask(ctx, id)
	if err != nil || snap == nil {
		return fmt.Sprintf("子智能体 %s 未找到", id), nil //nolint:nilerr // 任务未找到或读取失败作为业务提示返回
	}
	if snap.Status == types.TaskDone || snap.Status == types.TaskFailed {
		return fmt.Sprintf("子智能体 %s 已结束", id), nil
	}

	delivered := false
	// 1. 直连尝试（同节点快速通道，R2-14/R3-04）
	if a.subagentCtrl != nil {
		if a.subagentCtrl.TrySteer("sub-"+id, text) {
			delivered = true
		} else if a.subagentCtrl.TrySteer(id, text) {
			delivered = true
		}
	}
	// 2. 经 Blackboard 独立控制通道广播（R3-04），不走事件流消除回环
	if a.handoffPoster.SendTaskControl(id, types.TaskControl{
		Kind: types.TaskControlSteer,
		Text: text,
	}) {
		delivered = true
	}

	if delivered {
		return fmt.Sprintf("已成功向子智能体 %s 送达消息", id), nil
	}
	return fmt.Sprintf("子智能体 %s 未找到活跃执行器", id), nil
}

// AgentStop 中止指定子智能体（R3-04）。
func (a *Agent) AgentStop(ctx context.Context, id string) (string, error) {
	if a.handoffPoster == nil {
		return "", apperr.New(apperr.CodeInternal, "agent_stop: handoffPoster not injected")
	}
	snap, err := a.handoffPoster.PeekTask(ctx, id)
	if err != nil || snap == nil {
		return fmt.Sprintf("子智能体 %s 未找到", id), nil //nolint:nilerr // 任务未找到或读取失败作为业务提示返回
	}
	if snap.Status == types.TaskDone || snap.Status == types.TaskFailed {
		return fmt.Sprintf("子智能体 %s 已结束", id), nil
	}

	// 1. 经 Blackboard 独立控制通道发送 cancel（R3-04）
	delivered := a.handoffPoster.SendTaskControl(id, types.TaskControl{
		Kind: types.TaskControlCancel,
	})
	// 2. 直连停止子 Agent 并从 Pool 释放（R2-14/R3-04）
	if a.subagentCtrl != nil {
		if a.subagentCtrl.StopSession("sub-" + id) {
			delivered = true
		} else if a.subagentCtrl.StopSession(id) {
			delivered = true
		}
	}
	// 3. 将 Blackboard 任务置为 Failed
	if err := a.handoffPoster.FailTask(ctx, id, a.ID, []byte("cancelled by parent agent")); err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "agent_stop: cancel failed", err)
	}
	a.updateSubagentStatusCancelled(ctx, id)

	if delivered {
		return fmt.Sprintf("已成功中止子智能体 %s", id), nil
	}
	return fmt.Sprintf("子智能体 %s 未找到活跃执行器，已标记中止", id), nil
}

func (a *Agent) currentSecurityPolicy() policy.Policy {
	prof := policy.ProfileStandard
	if cfg := config.Get(); cfg != nil && cfg.Security.TrustProfile != "" {
		prof = policy.TrustProfile(cfg.Security.TrustProfile)
	}
	return policy.ProfilePolicy(prof)
}

func (a *Agent) recordSubagentRun(ctx context.Context, id, role, model, thinking, prompt string) {
	if a.extQuerier == nil {
		return
	}
	pPrompt := prompt
	if len(pPrompt) > 4000 {
		pPrompt = pPrompt[:4000]
	}
	nowMs := time.Now().UnixMilli()
	if _, err := a.extQuerier.ExecContext(ctx, `
		INSERT INTO subagent_runs (id, parent_session_id, child_session_id, task_id, parent_agent_id, agent_type, model, thinking, entry, status, prompt, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'spawn_agents', 'running', ?, ?)`,
		id, a.sCtx.SessionID, "sub-"+id, id, a.ID, role, model, thinking, pPrompt, nowMs); err != nil {
		slog.WarnContext(ctx, "agent: record subagent_runs failed", "id", id, "err", err)
	}
}

func (a *Agent) updateSubagentStatusCancelled(ctx context.Context, id string) {
	if a.extQuerier == nil {
		return
	}
	nowMs := time.Now().UnixMilli()
	if _, err := a.extQuerier.ExecContext(ctx, `
		UPDATE subagent_runs SET status = 'cancelled', finished_at = ? WHERE id = ? OR task_id = ?`,
		nowMs, id, id); err != nil {
		slog.WarnContext(ctx, "agent: update subagent_runs cancelled failed", "id", id, "err", err)
	}
}
