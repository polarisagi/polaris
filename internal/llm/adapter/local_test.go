package adapter

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestNewLocalAdapter_DefaultState(t *testing.T) {
	a := NewLocalAdapter()
	if a.ModelID() != "local:unloaded" {
		t.Errorf("expected default ModelID 'local:unloaded', got %q", a.ModelID())
	}
	caps := a.Capabilities()
	if !caps.SupportsStreaming {
		t.Error("expected SupportsStreaming=true")
	}
	if caps.SupportsTools {
		t.Error("expected SupportsTools=false (llama.cpp has no native tool_call protocol)")
	}
	if a.Tokenizer() == nil {
		t.Error("expected non-nil Tokenizer")
	}
}

func TestToLocalMessages_Conversion(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
	}
	out := toLocalMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "you are helpful" {
		t.Errorf("unexpected first message: %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "hi" {
		t.Errorf("unexpected second message: %+v", out[1])
	}
}

// TestLocalAdapter_InferWithoutLoadedModelReturnsError 验证未加载模型时
// Infer 返回明确错误而非 panic——无论底层 dylib 是否以 tier1 构建都应成立
// （bindLlamaInfer 优雅降级 + Rust 侧"no model loaded"业务错误路径共同保证）。
func TestLocalAdapter_InferWithoutLoadedModelReturnsError(t *testing.T) {
	a := NewLocalAdapter()
	_, err := a.Infer(context.Background(), []types.Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error: no model loaded (or tier1 symbols unavailable)")
	}
}

func TestLocalAdapter_StreamInferWithoutLoadedModelEmitsErrorEvent(t *testing.T) {
	a := NewLocalAdapter()
	ch, err := a.StreamInfer(context.Background(), []types.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("StreamInfer itself should not error synchronously: %v", err)
	}
	ev, ok := <-ch
	if !ok {
		t.Fatal("expected at least one event before channel close")
	}
	if ev.Type != types.StreamError {
		t.Errorf("expected StreamError event, got type=%v content=%q", ev.Type, ev.Content)
	}
	if _, stillOpen := <-ch; stillOpen {
		t.Error("expected channel to be closed after error event")
	}
}

func TestLocalAdapter_LocalStatusGraceful(t *testing.T) {
	a := NewLocalAdapter()
	status, err := a.LocalStatus(context.Background())
	if !ffi.LlamaAvailable() {
		if err == nil {
			t.Fatal("expected error when llama_infer symbols unavailable")
		}
		return
	}
	if err != nil {
		t.Fatalf("LocalStatus should not error when symbols available: %v", err)
	}
	if status.Loaded {
		t.Error("expected Loaded=false: no LoadModel call was made in this test")
	}
}

// TestLocalAdapter_ProbeGraceful 验证 Probe()（M11 §5.3 Tier3 内存守卫依赖）
// 在 tier1 符号不可用时优雅报错、可用时返回一致的未加载状态 + 非零内存读数。
func TestLocalAdapter_ProbeGraceful(t *testing.T) {
	a := NewLocalAdapter()
	result, err := a.Probe(context.Background())
	if !ffi.LlamaAvailable() {
		if err == nil {
			t.Fatal("expected error when llama_infer symbols unavailable")
		}
		return
	}
	if err != nil {
		t.Fatalf("Probe should not error when symbols available: %v", err)
	}
	if result.ModelLoadable {
		t.Error("expected ModelLoadable=false: no LoadModel call was made in this test")
	}
	if result.UsedMemoryBytes == 0 {
		t.Error("expected non-zero UsedMemoryBytes from probe.MemoryProbe()")
	}
}
