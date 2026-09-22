package agent

import (
	"context"
	"testing"

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

	_, err := a.doStreamInfer(context.Background(), ch)
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

	resp, err := a.doStreamInfer(context.Background(), ch)
	if err == nil {
		t.Fatalf("expected error, got nil (resp=%+v) — StreamCancelled 被静默当作成功", resp)
	}
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Errorf("expected error code %s, got err: %v", apperr.CodeCancelled, err)
	}
}
