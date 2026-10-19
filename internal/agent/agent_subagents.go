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
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

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

	// 1. 并发上限检查
	maxConcurrent := pol.SubagentMaxConcurrent
	if maxConcurrent > 0 && len(args.Tasks) > maxConcurrent {
		return nil, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("spawn_agents: task count %d exceeds concurrency limit %d for profile %s",
				len(args.Tasks), maxConcurrent, pol.Profile))
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
				return nil, apperr.Wrap(apperr.CodeForbidden, fmt.Sprintf("spawn_agents: task[%d] model %q rejected by policy", i, model), err)
			}
		}

		childID := fmt.Sprintf("subagent-%s-%d-%s", a.ID, i, uuid.NewString()[:8])
		childIDs = append(childIDs, childID)

		payload, err := json.Marshal(task)
		if err != nil {
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
			return nil, apperr.Wrap(apperr.CodeOf(pErr), "spawn_agents: post task", pErr)
		}

		a.recordSubagentRun(ctx, childID, role, model, task.Thinking, task.Prompt)
	}

	// 3. wait 模式处理
	waitMode := strings.ToLower(strings.TrimSpace(args.Wait))
	if waitMode == "none" {
		return childIDs, nil
	}

	// wait=all 场景：将子智能体集合写入上下文并挂起当前 Agent
	a.sCtx.Mu.Lock()
	a.sCtx.HandoffTaskIDs = childIDs
	if len(childIDs) > 0 {
		a.sCtx.HandoffTaskID = childIDs[0] // 兼容既有单值检查点
	}
	a.sCtx.Mu.Unlock()

	a.watchSubagentsCompletion(childIDs)
	a.asyncIntent(types.TriggerAwaitAgent)

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

// AgentMessage 向指定子智能体发送插话消息（ADR-0115 WP4 / WP2 steer 机制）。
func (a *Agent) AgentMessage(ctx context.Context, id, text string) (string, error) {
	if a.handoffPoster == nil {
		return "", apperr.New(apperr.CodeInternal, "agent_message: handoffPoster not injected")
	}
	ev := types.AgentStreamEvent{
		Type:     types.AgentStreamEventNotice,
		Content:  text,
		AgentID:  id,
		ParentID: a.ID,
		IsNested: true,
	}
	a.handoffPoster.PublishTaskEvent(id, ev)
	return fmt.Sprintf("已成功向子智能体 %s 投递消息", id), nil
}

// AgentStop 中止指定子智能体。
func (a *Agent) AgentStop(ctx context.Context, id string) (string, error) {
	if a.handoffPoster == nil {
		return "", apperr.New(apperr.CodeInternal, "agent_stop: handoffPoster not injected")
	}
	if err := a.handoffPoster.FailTask(ctx, id, a.ID, []byte("cancelled by parent agent")); err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "agent_stop: cancel failed", err)
	}
	a.updateSubagentStatusCancelled(ctx, id)
	return fmt.Sprintf("已成功中止子智能体 %s", id), nil
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

func (a *Agent) watchSubagentsCompletion(childIDs []string) { //nolint:gocyclo
	if a.handoffPoster == nil || len(childIDs) == 0 {
		return
	}
	watchCtx, cancel := context.WithCancel(a.ctx)
	a.registerHandoffWatcher(cancel)

	concurrent.SafeGo(watchCtx, "agent.subagents_watcher", func(ctx context.Context) {
		defer a.cancelHandoffWatcher()

		for _, cid := range childIDs {
			cID := cid
			cEvents, cancelSub := a.handoffPoster.SubscribeTaskEvents(cID)
			defer cancelSub()
			concurrent.SafeGo(ctx, "agent.subagents_event_forwarder", func(subCtx context.Context) {
				for {
					select {
					case <-subCtx.Done():
						return
					case ev, ok := <-cEvents:
						if !ok {
							return
						}
						a.publishStreamEvent(ev)
					}
				}
			})
		}

		events, err := a.handoffPoster.Subscribe(ctx)
		if err != nil {
			a.pollSubagentsCompletion(ctx, childIDs)
			return
		}

		if a.peekAllSubagentsDone(ctx, childIDs) {
			a.asyncIntent(types.TriggerAgentHandoffDone)
			return
		}

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-events:
				if a.peekAllSubagentsDone(ctx, childIDs) {
					a.asyncIntent(types.TriggerAgentHandoffDone)
					return
				}
			case <-ticker.C:
				if a.peekAllSubagentsDone(ctx, childIDs) {
					a.asyncIntent(types.TriggerAgentHandoffDone)
					return
				}
			}
		}
	})
}

func (a *Agent) pollSubagentsCompletion(ctx context.Context, childIDs []string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.peekAllSubagentsDone(ctx, childIDs) {
				a.asyncIntent(types.TriggerAgentHandoffDone)
				return
			}
		}
	}
}

func (a *Agent) peekAllSubagentsDone(ctx context.Context, childIDs []string) bool {
	for _, id := range childIDs {
		snap, err := a.handoffPoster.PeekTask(ctx, id)
		if err != nil || snap == nil {
			return false
		}
		if snap.Status != types.TaskDone && snap.Status != types.TaskFailed {
			return false
		}
	}
	return true
}

func (a *Agent) truncateAndOffloadResult(ctx context.Context, taskID, resStr string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 4000
	}
	if len(resStr) <= maxChars {
		return resStr
	}

	header := fmt.Sprintf("[output truncated: %d chars total; full output not retained]\n", len(resStr))
	if a.toolOffloader != nil {
		partKey := a.memoryPartitionKey()
		if offID, err := a.toolOffloader.Offload(ctx, partKey, []byte(resStr)); err == nil {
			ref := fmt.Sprintf("read_tool_ref(task_id=%q, id=%q)", partKey, offID)
			header = fmt.Sprintf("[output truncated: %d chars total; full output: %s]\n", len(resStr), ref)
		} else {
			slog.WarnContext(ctx, "agent: subagent result offload failed", "task_id", taskID, "err", err)
		}
	}
	return header + util.ElideMiddle(resStr, maxChars-len(header))
}
