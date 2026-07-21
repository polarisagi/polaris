package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// outboxSeqCounter 进程内单调递增计数器，与纳秒时间戳组合生成幂等键后缀。
// 单独使用 time.Now().UnixNano() 不足以保证唯一：同一 goroutine 内背靠背的
// 两次调用（如 recordLLMFillEffectMemory 的 reflect 分支紧接着触发
// consolidate 分支）可能落在同一操作系统时钟粒度内，UnixNano() 返回相同值
// （已被 TestOutboxUniqueSuffix_Unique 实测命中）。加一个原子计数器可在
// 时钟精度不足时兜底，保证进程内任意两次调用绝不相同。
// 命名/写法与 cmd/polaris/boot_events.go 的 eventSeqTiebreaker 保持一致
// （同为"纯粹避免碰撞的单调序号"，非业务共享状态，故豁免 gochecknoglobals）。
//
//nolint:gochecknoglobals
var outboxSeqCounter atomic.Uint64

// outboxUniqueSuffix 返回一个真正唯一的幂等键后缀（纳秒时间戳 + 单调序号）。
// 2026-07-22 一致性审查修复背景：本文件多处 Outbox 幂等键此前固定为
// "{sessionID}:{阶段}:{agentID}" 形状——由于同一 chat session 的 sessionID/
// AgentID 在其*整个生命周期*内保持不变（internal/agent/agent.go NewAgent：
// sCtx.SessionID = id = AgentID），该键对同一会话的每一轮对话都完全相同。
// outbox.idempotency_key 是 UNIQUE 约束列（002_outbox.sql），意味着这类
// 投影事件（perceive/plan/exec/reflect 记忆投影 + 记忆蒸馏触发）此前只有
// *会话的第一轮*能成功写入 outbox，第二轮起全部因约束冲突被静默丢弃
// （`_ = ...Write(...)` 吞掉了错误）——多轮对话场景下记忆投影/蒸馏实际上从
// 第二轮起就从未真正触发过。这里的调用点都是同步执行一次、无重试语义，
// 用纳秒时间戳 + 单调序号即可保证每次真实投影都不会与历史记录或同一进程内
// 其他调用冲突，不需要引入更复杂的跨 Agent 实例全局幂等状态机制。
func outboxUniqueSuffix() string {
	seq := outboxSeqCounter.Add(1)
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(seq, 10)
}

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
				TaskID:    a.memoryPartitionKey(), // GD-14-001：命名空间内共享，未设置时等于 SessionID
				Payload:   a.sCtx.ExecuteResult,
				CreatedAt: time.Now(),
			})
			if a.outboxWriter != nil {
				ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
					ID:        eventID,
					Type:      "execution_completed",
					TaskID:    a.memoryPartitionKey(),
					Payload:   a.sCtx.ExecuteResult,
					CreatedAt: time.Now(),
				}, a.sCtx.SessionID+":exec:"+a.sCtx.AgentID+":"+outboxUniqueSuffix())
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
	var toolCalls []types.InferToolCall

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
		case types.StreamToolCall:
			// adapter 侧（stream.go/anthropic_request.go/google_request.go）已把
			// 原生 tool_use/tool_calls 事件统一打包为 {"id","name","input"} JSON，
			// 这里是全链路中第一个真正消费 StreamToolCall 的地方——此前该事件类型
			// 只被产出、从未被读取，原生 function-calling 通路因此实际死管线。
			var tc struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if jsonErr := json.Unmarshal([]byte(ev.Content), &tc); jsonErr != nil {
				slog.Warn("agent: doStreamInfer failed to parse StreamToolCall payload, skipping", "err", jsonErr)
				continue
			}
			toolCalls = append(toolCalls, types.InferToolCall{ID: tc.ID, Name: tc.Name, Input: tc.Input})
			a.publishStreamEvent(types.AgentStreamEvent{
				Type:       types.AgentStreamEventToolCall,
				TaintLevel: a.sCtx.GlobalTaintLevel,
				ToolName:   tc.Name,
				ToolInput:  tc.Input,
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
		ToolCalls:        toolCalls,
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
			TaskID:    a.memoryPartitionKey(),
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "task_perceived",
				TaskID:    a.memoryPartitionKey(),
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":perceive:"+a.sCtx.AgentID+":"+outboxUniqueSuffix())
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
			TaskID:    a.memoryPartitionKey(),
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "plan_generated",
				TaskID:    a.memoryPartitionKey(),
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":plan:"+a.sCtx.AgentID+":"+outboxUniqueSuffix())
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
			TaskID:    a.memoryPartitionKey(),
			Payload:   []byte(content),
			CreatedAt: time.Now(),
		})
		if a.outboxWriter != nil {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicProject, "project", types.Event{
				ID:        eventID,
				Type:      "reflection_completed",
				TaskID:    a.memoryPartitionKey(),
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			}, a.sCtx.SessionID+":reflect:"+a.sCtx.AgentID+":"+outboxUniqueSuffix())
			_ = a.outboxWriter.Write(ctx, ev)
		}
		// 触发 Episodic → Semantic 4 阶段记忆蒸馏（ConsolidationPipeline，M5 §4）
		if a.outboxWriter != nil && a.sCtx.SessionID != "" {
			ev, _ := protocol.NewOutboxEvent(protocol.TopicMemoryConsolidate, "memory_consolidate", map[string]string{"session_id": a.sCtx.SessionID}, a.sCtx.SessionID+":consolidate:"+outboxUniqueSuffix())
			_ = a.outboxWriter.Write(ctx, ev)
		}
	}
}

// reconstructReplayResponse 把 ReplayLLMCall.Response（TrajectoryRecorderImpl
// 从 EventLog 扫描重建的通用 map[string]any，键名与
// agent_execute_effect.go 写入 WriteLLMCallEvent 时的 respMap 完全对应：
// content/reasoning_content/usage/tool_calls）还原为 *types.ProviderResponse，
// 供崩溃恢复回放时替代真实 Provider 调用。
//
// usage/tool_calls 走 json 编解码往返而非逐字段类型断言：两者原始写入时是
// resp.Usage（types.Usage）/resp.ToolCalls（[]types.InferToolCall）的 Go 值，
// 经 json.Marshal 落盘、TrajectoryRecorderImpl 用 map[string]any 泛读回来，
// 字段名与原 Go 结构体字段名一致（两个类型均无 json tag，用默认导出字段名）；
// 重新 Marshal 该 map 再 Unmarshal 进目标类型，比手写字段映射更不易随
// Usage/InferToolCall 后续增删字段而漂移。
func reconstructReplayResponse(m map[string]any) *types.ProviderResponse {
	resp := &types.ProviderResponse{}
	if m == nil {
		return resp
	}
	if v, ok := m["content"].(string); ok {
		resp.Content = v
	}
	if v, ok := m["reasoning_content"].(string); ok {
		resp.ReasoningContent = v
	}
	if u, ok := m["usage"]; ok {
		if b, err := json.Marshal(u); err == nil {
			_ = json.Unmarshal(b, &resp.Usage)
		}
	}
	if tc, ok := m["tool_calls"]; ok {
		if b, err := json.Marshal(tc); err == nil {
			_ = json.Unmarshal(b, &resp.ToolCalls)
		}
	}
	return resp
}
