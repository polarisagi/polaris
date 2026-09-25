package adapter

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestNewLocalAdapter_DefaultState(t *testing.T) {
	a := NewLocalAdapter(nil)
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
	a := NewLocalAdapter(nil)
	_, err := a.Infer(context.Background(), []types.Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error: no model loaded (or tier1 symbols unavailable)")
	}
}

func TestLocalAdapter_StreamInferWithoutLoadedModelEmitsErrorEvent(t *testing.T) {
	a := NewLocalAdapter(nil)
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
	a := NewLocalAdapter(nil)
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
	a := NewLocalAdapter(nil)
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

// ADR-0101 决策五：无原生 tools 的本地适配器须把 WithTools 下发的定义渲染成文本，
// 插在前导 system 之后，否则上层只列工具名时本地模型拿不到参数 schema。
func TestWithToolsAsText(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: "persona"},
		{Role: "system", Content: "plan"},
		{Role: "user", Content: "list files"},
	}
	tools := []types.ToolSchema{{Name: "ls", Description: "list dir", Parameters: map[string]any{"type": "object"}}}
	out := withToolsAsText(msgs, tools)
	if len(out) != 4 || out[2].Role != "system" || !strings.Contains(out[2].Content, "ls: list dir") ||
		!strings.Contains(out[2].Content, `{"type":"object"}`) {
		t.Fatalf("工具定义应插在前导 system 之后：%+v", out)
	}
	if len(msgs) != 3 {
		t.Fatal("不得修改入参切片")
	}
	if got := withToolsAsText(msgs, nil); len(got) != 3 {
		t.Fatal("无工具时原样返回")
	}
}
