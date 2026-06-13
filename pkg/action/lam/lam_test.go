package lam

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockProvider 顺序返回预设响应（线程安全）。
type mockProvider struct {
	mu        sync.Mutex
	responses []string
	idx       int
}

func (m *mockProvider) Infer(_ context.Context, _ []protocol.Message, _ ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.responses) {
		return nil, fmt.Errorf("mockProvider: no more responses")
	}
	resp := m.responses[m.idx]
	m.idx++
	return &protocol.ProviderResponse{Content: resp}, nil
}

func (m *mockProvider) StreamInfer(_ context.Context, _ []protocol.Message, _ ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	ch := make(chan protocol.StreamEvent)
	close(ch)
	return ch, nil
}

func (m *mockProvider) Capabilities() protocol.ProviderCapabilities {
	return protocol.ProviderCapabilities{}
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) ModelID() string                      { return "mock" }

// ── ExecuteAction 基础路径 ───────────────────────────────────────────────────

func TestExecuteAction_Disabled(t *testing.T) {
	e := NewComputerUseEngine(LAMConfig{Enabled: false}, &mockProvider{}, nil)
	result, err := e.ExecuteAction(context.Background(), "click button", &ScreenState{DOM: "<div/>"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("disabled LAM should return Success=false")
	}
	if result.Error == "" {
		t.Error("disabled LAM should return non-empty Error message")
	}
}

func TestExecuteAction_NilProvider(t *testing.T) {
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, nil, nil)
	_, err := e.ExecuteAction(context.Background(), "click", &ScreenState{DOM: "<div/>"})
	if err == nil {
		t.Fatal("nil provider should return error")
	}
}

func TestExecuteAction_NilScreenState(t *testing.T) {
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, &mockProvider{}, nil)
	_, err := e.ExecuteAction(context.Background(), "click", nil)
	if err == nil {
		t.Fatal("nil screenState should return error")
	}
}

// ── DryRun 模式（executor=nil）─────────────────────────────────────────────

func TestExecuteAction_DryRun_ReturnsActionJSON(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":"left_click","coordinate":[100,200],"reasoning":"button found"}`},
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true, ResolverModel: "mock"}, provider, nil)

	state := &ScreenState{DOM: "<button id='ok'>OK</button>", Width: 1280, Height: 720}
	result, err := e.ExecuteAction(context.Background(), "click OK button", state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("dry-run should return Success=true, got error: %s", result.Error)
	}
	if len(result.Output) == 0 {
		t.Error("dry-run should return non-empty action JSON in Output")
	}

	// 验证 reasoning 字段已被过滤，executor 只收到 action+coordinate
	var args map[string]any
	if err := json.Unmarshal(result.Output, &args); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, ok := args["reasoning"]; ok {
		t.Error("reasoning field should be stripped before passing to executor")
	}
	if args["action"] != "left_click" {
		t.Errorf("expected action=left_click, got %v", args["action"])
	}
}

// ── 带 executor 的真实执行路径 ──────────────────────────────────────────────

func TestExecuteAction_WithExecutor_Success(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":"screenshot"}`},
	}
	executed := false
	executorFn := func(_ context.Context, input []byte) ([]byte, error) {
		executed = true
		return []byte(`{"status":"ok"}`), nil
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, executorFn)

	result, err := e.ExecuteAction(context.Background(), "take screenshot", &ScreenState{DOM: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !executed {
		t.Error("executor should have been called")
	}
	if !result.Success {
		t.Errorf("expected Success=true, got error: %s", result.Error)
	}
}

func TestExecuteAction_WithExecutor_Error(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":"key","text":"Enter"}`},
	}
	executorFn := func(_ context.Context, _ []byte) ([]byte, error) {
		return nil, fmt.Errorf("display not found")
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, executorFn)

	result, err := e.ExecuteAction(context.Background(), "press enter", &ScreenState{DOM: ""})
	if err != nil {
		t.Fatalf("unexpected error from ExecuteAction itself: %v", err)
	}
	if result.Success {
		t.Error("executor error should propagate as Success=false")
	}
	if result.Error == "" {
		t.Error("executor error message should be set")
	}
}

// ── VLM 响应解析错误 ──────────────────────────────────────────────────────────

func TestExecuteAction_InvalidVLMResponse(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`not-valid-json`},
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, nil)

	_, err := e.ExecuteAction(context.Background(), "click", &ScreenState{DOM: "<div/>"})
	if err == nil {
		t.Fatal("invalid VLM JSON should return error")
	}
}

func TestExecuteAction_EmptyAction(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":""}`},
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, nil)

	_, err := e.ExecuteAction(context.Background(), "do nothing", &ScreenState{DOM: "<div/>"})
	if err == nil {
		t.Fatal("empty action field should return error")
	}
}

// ── Vision 路径（截图在 2MB 以内）─────────────────────────────────────────

func TestExecuteAction_VisionPath_SmallScreenshot(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":"mouse_move","coordinate":[50,50]}`},
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, nil)

	// 1 字节截图 → useVision=true
	state := &ScreenState{
		ScreenshotBytes: []byte{0x89}, // 极小"截图"
		Width:           800,
		Height:          600,
		DOM:             "<canvas/>",
	}
	result, err := e.ExecuteAction(context.Background(), "move mouse", state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("vision path should succeed, got: %s", result.Error)
	}
}

func TestExecuteAction_VisionPath_OversizedScreenshot_DegradesToDOM(t *testing.T) {
	provider := &mockProvider{
		responses: []string{`{"action":"type","text":"hello"}`},
	}
	e := NewComputerUseEngine(LAMConfig{Enabled: true}, provider, nil)

	// 超出 2MB → useVision=false，降级为 DOM-only
	oversized := make([]byte, maxScreenshotBytesFull+1)
	state := &ScreenState{
		ScreenshotBytes: oversized,
		DOM:             "<input type='text'/>",
	}
	result, err := e.ExecuteAction(context.Background(), "type hello", state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("oversized screenshot should degrade gracefully, got: %s", result.Error)
	}
}
