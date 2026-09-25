package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// doStreamInfer 消费 Provider 流并累积完整响应。
//
// audience 决定文本增量是否作为用户回复发布（ADR-0098 决策一，par_inv_06）：
// Perceive/Plan/Reflect 的输出是给解析器的结构化填空，逐 token 推给用户即
// 2026-09-24「回复里夹带 DAG JSON」缺陷本身。思考链照常发布，前端独立渲染。
func (a *Agent) doStreamInfer(ctx context.Context, ch <-chan types.StreamEvent, audience protocol.LLMAudience) (*types.ProviderResponse, error) {
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
			if audience == protocol.AudienceUser {
				a.publishStreamEvent(types.AgentStreamEvent{
					Type:       types.AgentStreamEventToken,
					Content:    ev.Content,
					TaintLevel: a.sCtx.GlobalTaintLevel,
				})
			}
		case types.StreamToolCall:
			if tc, ok := a.acceptStreamToolCall(ev.Content, audience); ok {
				toolCalls = append(toolCalls, tc)
			}
		case types.StreamSystemNotice:
			// 跨 Model Pool 降级提示（GD-13-005）：只透传给前端展示，
			// **不**写进 content/reasoning——它不是模型输出，混进正文会污染
			// 助手回复内容与后续轮次的消息历史。
			a.publishStreamEvent(types.AgentStreamEvent{
				Type:       types.AgentStreamEventNotice,
				Content:    ev.Content,
				TaintLevel: a.sCtx.GlobalTaintLevel,
			})
		case types.StreamError:
			if inferErr == nil {
				inferErr = apperr.New(apperr.CodeProviderExhausted, ev.Content)
			}
		case types.StreamCancelled:
			// 流被中断（ctx 取消或 StreamBudgetGuard 硬阻断）必须成为错误，否则 ch 关闭时
			// content/inferErr 双空，被误判为"成功但空"。
			if inferErr == nil {
				inferErr = apperr.New(apperr.CodeCancelled, "推理流被中断: "+ev.Content).WithRetryAfter(5)
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

// acceptStreamToolCall 解析 adapter 统一打包的 {"id","name","input"} 工具调用。
//
// adapter 侧（stream.go/anthropic_request.go/google_request.go）已把原生 tool_use/
// tool_calls 事件统一打包；这里是全链路第一个真正消费 StreamToolCall 的地方——此前
// 该事件只被产出、从未被读取，原生 function-calling 通路实际是死管线。
// 规划阶段的工具调用只是意图：尚未过 S_VALIDATE，可能被拒。此前在此发布 ToolCall
// 事件，前端对被拒工具也显示"正在执行"，真正执行时（agent_execute_dag.go）又发一次
// （2026-09-25 实测）。受众原则同文本增量（ADR-0098 决策一）。
func (a *Agent) acceptStreamToolCall(payload string, audience protocol.LLMAudience) (types.InferToolCall, bool) {
	var tc struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(payload), &tc); err != nil {
		slog.Warn("agent: doStreamInfer failed to parse StreamToolCall payload, skipping", "err", err)
		return types.InferToolCall{}, false
	}
	if audience == protocol.AudienceUser {
		a.publishStreamEvent(types.AgentStreamEvent{
			Type:       types.AgentStreamEventToolCall,
			TaintLevel: a.sCtx.GlobalTaintLevel,
			ToolName:   tc.Name,
			ToolInput:  tc.Input,
		})
	}
	return types.InferToolCall{ID: tc.ID, Name: tc.Name, Input: tc.Input}, true
}
