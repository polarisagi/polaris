package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// executeDeterministicEffect 处理 executeEffect 的非 LLM 效应分支，从
// agent_execute_effect.go 拆出（R7 文件行数治理，2026-07-07），逻辑与拆分前完全一致。
//
// handled=true 表示该分支已自行完成状态机推进（S_VALIDATE/S_EXECUTE 均通过
// runValidateDAG/runExecuteDAG 内部 SendIntent 推进 FSM），调用方应直接
// return err，不再走 executeEffect 末尾的统一 stateToTriggerMap 分发，
// 避免双重推进。
func (a *Agent) executeDeterministicEffect(ctx context.Context, effect protocol.Effect) (nextState types.State, err error, handled bool) {
	detEff, ok := effect.(protocol.DeterministicEffect)
	if !ok {
		return "", apperr.New(apperr.CodeInternal, "invalid DeterministicEffect type"), true
	}

	// S_VALIDATE 阶段拦截：调用 Agent 层四层校验（可访问 policyGate 与完整 sCtx）。
	// 此分支由 runValidateDAG 自行通过 SendIntent 推进 FSM（ValidateOk / ValidateFail），
	// 因此直接返回，不走 stateToTriggerMap 路径，避免双重推进。
	if a.sm.Current() == types.AgentStateValidate {
		// FastPath 空执行路径（SurpriseIndex 触发但无 LLM 生成 DAG）：nil DAGModel 直接放行。
		// runValidateDAG 对 nil plan 会触发 L0 拦截，因此在进入前短路。
		if a.sCtx.DAGModel == nil {
			a.asyncIntent(types.TriggerValidateOk)
			return "", nil, true
		}
		if verr := a.runValidateDAG(ctx); verr != nil {
			// 业务校验失败会触发 ValidateFail，不应被视为系统级致命错误导致 Run 崩溃退出
			slog.Debug("kernel: validate DAG", "err", verr)
		}
		return "", nil, true
	}

	// S_EXECUTE 阶段拦截：调用 Agent 层 DAG 执行（可访问 toolRegistry 与完整 sCtx）。
	// 同理，由 runExecuteDAG 自行推进 FSM（ExecuteDone / ExecuteFail）。
	if a.sm.Current() == types.AgentStateExecute {
		// runExecuteDAG 内负责在完成后将结果写入 a.sCtx.ExecuteResult
		execErr := a.runExecuteDAG(ctx)
		if execErr == nil && a.memory != nil && len(a.sCtx.ExecuteResult) > 0 {
			eventID := a.sm.NextEventID(a.sCtx.SessionID, "exec")
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        eventID,
				Type:      "execution_completed",
				Payload:   a.sCtx.ExecuteResult,
				CreatedAt: time.Now(),
			})
			if a.outboxWriter != nil {
				ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
					ID:        eventID,
					Type:      "execution_completed",
					TaskID:    a.sCtx.SessionID,
					Payload:   a.sCtx.ExecuteResult,
					CreatedAt: time.Now(),
				}, a.sCtx.SessionID+":exec:"+a.sCtx.AgentID)
				_ = a.outboxWriter.Write(ctx, ev)
			}
		}
		// 业务执行失败会触发 ExecuteFail，同样不抛出以免阻断状态机
		return "", nil, true
	}

	if detEff.Fn != nil {
		nextState, err = detEff.Fn(ctx, a.toProtocolCtx())
	}
	// err 在此处故意不包装：handled=false 时调用方 executeEffect 会在其末尾统一
	// 用 apperr.Wrap 包装后再返回给上层（与拆分前完全一致的错误传播路径）。
	return nextState, err, false //nolint:wrapcheck
}

// doStreamInfer 从拆分出 Stream 事件循环

func (a *Agent) doStreamInfer(ctx context.Context, ch <-chan types.StreamEvent) (*types.ProviderResponse, error) {
	var content strings.Builder
	var reasoning strings.Builder
	var usage types.Usage
	var inferErr error

	for ev := range ch {
		switch ev.Type {
		case types.StreamThinking:
			reasoning.WriteString(ev.Content)
			a.publishStreamEvent(types.AgentStreamEvent{
				Type:       types.AgentStreamEventThinking,
				Content:    ev.Content,
				TaintLevel: a.sCtx.GlobalTaintLevel,
			})
		case types.StreamTextDelta:
			content.WriteString(ev.Content)
			a.publishStreamEvent(types.AgentStreamEvent{
				Type:       types.AgentStreamEventToken,
				Content:    ev.Content,
				TaintLevel: a.sCtx.GlobalTaintLevel,
			})
		case types.StreamError:
			if inferErr == nil {
				inferErr = apperr.New(apperr.CodeProviderExhausted, ev.Content)
			}
		}
		if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
			usage.InputTokens = ev.Usage.InputTokens
			usage.OutputTokens = ev.Usage.OutputTokens
			usage.CacheHitTokens = ev.Usage.CacheHitTokens
		}
	}

	if inferErr != nil {
		return nil, inferErr
	}
	return &types.ProviderResponse{
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		Usage:            usage,
	}, nil
}

// recordLLMFillEffectMemory 是 executeEffect 中 HANDLE_MEM 标签体的拆出版本
// （R7 文件行数治理，2026-07-07），逻辑与拆分前完全一致：LLMFillEffect 分支
// 无论走 FastPath/PRM候选/标准单次推理哪条路径，最终都汇合到这里按 nextState
// 落盘对应的 episodic 事件 + outbox 投递。
//
// gocyclo 偏高是三个 nextState 分支（S_PERCEIVE_DONE/S_PLAN_DONE/S_REFLECT_DONE）
// 平铺并列导致，拆分前该复杂度计入 executeEffect 本身（已有 //nolint:gocyclo），
// 拆分只是物理搬迁，不应也不必在这里拆得更碎去规避圈复杂度检查。
//
//nolint:gocyclo
func (a *Agent) recordLLMFillEffectMemory(ctx context.Context, nextState types.State, resp *types.ProviderResponse) {
	// ReplayMode 物理短路：回放时不写副作用，防止双写 EventLog / Outbox。
	if protocol.IsReplaying() {
		return
	}
	// 成功完成感知，将用户意图作为事件写入记忆（由于当前缺失 TaskIntent，仅做预留演示）
	if nextState == "S_PERCEIVE_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
		var content string
		if resp != nil {
			content = resp.Content
		} else {
			content = `{"Goal":` + a.sCtx.RawIntentTS.MarshalJSONString() + `,"Complexity":0.1}`
		}
		eventID := a.sm.NextEventID(a.sCtx.SessionID, "perceive")
		a.writeEpisodicWithExtract(ctx, types.Event{
			ID:        eventID,
			Type:      "task_perceived",
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "task_perceived",
				TaskID:    a.sCtx.SessionID,
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":perceive:"+a.sCtx.AgentID)
			_ = a.outboxWriter.Write(ctx, ev)
		}
	}

	// 成功完成计划，写入计划记忆
	if nextState == "S_PLAN_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
		// 恢复步骤预算 (ISSUE-08)
		if a.sCtx.InitialMaxStepsLimit > 0 {
			a.sCtx.MaxStepsLimit = a.sCtx.InitialMaxStepsLimit
		}

		var content string
		if resp != nil {
			content = resp.Content
		} else if a.sCtx.DAGModel != nil {
			planBytes, _ := json.Marshal(a.sCtx.DAGModel)
			content = string(planBytes)
		}
		eventID := a.sm.NextEventID(a.sCtx.SessionID, "plan")
		a.writeEpisodicWithExtract(ctx, types.Event{
			ID:        eventID,
			Type:      "plan_generated",
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "plan_generated",
				TaskID:    a.sCtx.SessionID,
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":plan:"+a.sCtx.AgentID)
			_ = a.outboxWriter.Write(ctx, ev)
		}
	}

	// 成功完成反思，写入反思记忆，并保存 ReasoningState
	if nextState == "S_REFLECT_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
		var content string
		if resp != nil {
			content = resp.Content
		}
		// 保存至内存上下文以供跨轮次携带
		a.sCtx.ReasoningState = []byte(content)

		eventID := a.sm.NextEventID(a.sCtx.SessionID, "reflect")
		a.writeEpisodicWithExtract(ctx, types.Event{
			ID:        eventID,
			Type:      "reflection_completed",
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "reflection_completed",
				TaskID:    a.sCtx.SessionID,
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":reflect:"+a.sCtx.AgentID)
			_ = a.outboxWriter.Write(ctx, ev)
		}
		// 触发 Episodic → Semantic 4 阶段记忆蒸馏（ConsolidationPipeline，M5 §4）
		if a.outboxWriter != nil && a.sCtx.SessionID != "" {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicMemoryConsolidate, "memory_consolidate", map[string]string{"session_id": a.sCtx.SessionID}, a.sCtx.SessionID+":consolidate")
			_ = a.outboxWriter.Write(ctx, ev)
		}
	}
}
