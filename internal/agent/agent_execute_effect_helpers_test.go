package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestDoStreamInfer_StreamError(t *testing.T) {
	a := &Agent{} // minimal mock for testing doStreamInfer

	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{
		Type:    types.StreamError,
		Content: "simulated stream error",
	}
	close(ch)

	_, err := a.doStreamInfer(context.Background(), ch, protocol.AudienceInternal)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !apperr.IsCode(err, apperr.CodeProviderExhausted) {
		t.Errorf("expected error code %s, got err: %v", apperr.CodeProviderExhausted, err)
	}
}

// TestDoStreamInfer_StreamCancelled 复现 2026-09-22 empty_response 根因：流被
// router_stream.go wrapStreamChannel 的 ctx.Done() 分支中断（StreamCancelled）
// 且此前未产出任何 token 时，doStreamInfer 必须返回错误，不能返回
// (空 content, nil error)——那会被 orchestrator_interactive.go 误判为
// "推理成功但无内容"，展示成无法追溯根因的通用报错。
func TestDoStreamInfer_StreamCancelled(t *testing.T) {
	a := &Agent{}

	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{
		Type:    types.StreamCancelled,
		Content: "context deadline exceeded",
	}
	close(ch)

	resp, err := a.doStreamInfer(context.Background(), ch, protocol.AudienceInternal)
	if err == nil {
		t.Fatalf("expected error, got nil (resp=%+v) — StreamCancelled 被静默当作成功", resp)
	}
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Errorf("expected error code %s, got err: %v", apperr.CodeCancelled, err)
	}
}

// TestDoStreamInfer_AudienceGatesTokens 复现 2026-09-24 缺陷（ADR-0098）：Plan 阶段
// 的 JSON 草稿被逐 token 推给用户。内部受众只累积 content 供解析，绝不发布 token；
// 思考链不受受众约束。
func TestDoStreamInfer_AudienceGatesTokens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		audience   protocol.LLMAudience
		wantTokens int
	}{
		{"内部阶段不外泄", protocol.AudienceInternal, 0},
		{"回复阶段实时推送", protocol.AudienceUser, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{sCtx: &fsm.StateContext{}, streamSubs: map[uint64]chan types.AgentStreamEvent{}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sub := a.SubscribeStream(ctx)

			ch := make(chan types.StreamEvent, 4)
			ch <- types.StreamEvent{Type: types.StreamThinking, Content: "思考"}
			ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: `{"nodes":`}
			ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: `[]}`}
			// 规划阶段的工具调用意图同样只对 User 受众发布（尚未过校验，可能被拒）。
			ch <- types.StreamEvent{Type: types.StreamToolCall, Content: `{"id":"c1","name":"bash","input":{}}`}
			close(ch)

			resp, err := a.doStreamInfer(ctx, ch, tc.audience)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if resp.Content != `{"nodes":[]}` {
				t.Fatalf("content 必须完整累积供 OnSuccess 解析，得到 %q", resp.Content)
			}
			tokens, thinking, toolCalls := 0, 0, 0
			for len(sub) > 0 {
				switch (<-sub).Type {
				case types.AgentStreamEventToken:
					tokens++
				case types.AgentStreamEventThinking:
					thinking++
				case types.AgentStreamEventToolCall:
					toolCalls++
				}
			}
			if tokens != tc.wantTokens {
				t.Errorf("token 事件 %d 条，期望 %d", tokens, tc.wantTokens)
			}
			if wantTC := min(tc.wantTokens, 1); toolCalls != wantTC {
				t.Errorf("工具调用事件 %d 条，期望 %d", toolCalls, wantTC)
			}
			if len(resp.ToolCalls) != 1 {
				t.Errorf("工具调用必须累积进响应供规划解析，得到 %d", len(resp.ToolCalls))
			}
			if thinking != 1 {
				t.Errorf("思考链不受受众约束，期望 1 条，得到 %d", thinking)
			}
		})
	}
}
