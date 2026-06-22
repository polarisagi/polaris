package planner

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── mock provider ──────────────────────────────────────────────────────────

type mockProvider struct {
	resp *types.ProviderResponse
	err  error
}

func (m *mockProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return m.resp, m.err
}

func (m *mockProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}

func (m *mockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockProvider) ModelID() string                      { return "mock" }

// ── parseTestScore ─────────────────────────────────────────────────────────

func TestParseTestScore_Empty(t *testing.T) {
	// 空输出 → 0.5（无法判断，给中等分）
	if got := parseTestScore(nil); got != 0.5 {
		t.Errorf("got %f, want 0.5", got)
	}
}

func TestParseTestScore_NoTestFiles(t *testing.T) {
	if got := parseTestScore([]byte("no test files")); got != 0.5 {
		t.Errorf("got %f, want 0.5", got)
	}
}

func TestParseTestScore_AllPass(t *testing.T) {
	out := `{"Action":"pass","Test":"TestA"}
{"Action":"pass","Test":"TestB"}`
	// 2 pass 0 fail → 0.5 + 0.5*(2/2) = 1.0
	if got := parseTestScore([]byte(out)); got != 1.0 {
		t.Errorf("got %f, want 1.0", got)
	}
}

func TestParseTestScore_MixedResults(t *testing.T) {
	out := `{"Action":"pass","Test":"TestA"}
{"Action":"fail","Test":"TestB"}`
	// 1 pass 1 fail → 0.5 + 0.5*(1/2) = 0.75
	if got := parseTestScore([]byte(out)); got != 0.75 {
		t.Errorf("got %f, want 0.75", got)
	}
}

func TestParseTestScore_AllFail(t *testing.T) {
	out := `{"Action":"fail","Test":"TestX"}
{"Action":"fail","Test":"TestY"}`
	// 0 pass 2 fail → 0.5 + 0.5*(0/2) = 0.5
	if got := parseTestScore([]byte(out)); got != 0.5 {
		t.Errorf("got %f, want 0.5", got)
	}
}

// ── NewPlannerPool ──────────────────────────────────────────────────────────

func TestNewPlannerPool(t *testing.T) {
	ch := make(chan protocol.MemoryWhisper, 1)
	pool := NewPlannerPool("fix bug", "code_act", nil, ch)
	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
	if pool.goal != "fix bug" {
		t.Errorf("goal: got %q, want %q", pool.goal, "fix bug")
	}
	if pool.taskType != "code_act" {
		t.Errorf("taskType: got %q, want %q", pool.taskType, "code_act")
	}
	if pool.whisperChan == nil {
		t.Error("whisperChan should be set")
	}
}

// ── Run ────────────────────────────────────────────────────────────────────

func TestPlannerPool_Run_NilWhisperChan(t *testing.T) {
	// nil whisperChan → 立即返回，不阻塞
	pool := NewPlannerPool("goal", "plan", nil, nil)
	done := make(chan struct{})
	go func() {
		pool.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with nil whisperChan blocked")
	}
}

func TestPlannerPool_Run_NilProvider_FallbackEngineB(t *testing.T) {
	// taskType != "code_act" → workerEngineB；provider nil 时走 fallback content
	ch := make(chan protocol.MemoryWhisper, 10)
	pool := NewPlannerPool("plan something", "plan", nil, ch)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool.Run(ctx)
	if len(ch) == 0 {
		t.Error("expected at least one whisper from fallback, got none")
	}
}

func TestPlannerPool_Run_MockProvider_EngineB(t *testing.T) {
	// 有 provider 响应时，whisper 应被推送且 Source 正确
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "detailed plan"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	pool := NewPlannerPool("refactor", "plan", prov, ch)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool.Run(ctx)
	if len(ch) == 0 {
		t.Fatal("expected at least one whisper, got none")
	}
	w := <-ch
	if w.Source != "planner_pool" {
		t.Errorf("Source: got %q, want %q", w.Source, "planner_pool")
	}
	if w.Content == "" {
		t.Error("whisper content should not be empty")
	}
}

func TestPlannerPool_Run_BestScoreWins(t *testing.T) {
	// workerEngineB 有 provider 时返回 score=0.9；无 provider 时返回 0.1
	// 三个 worker 中只要有一个返回高分，最终应是高分内容
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "best plan"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	pool := NewPlannerPool("goal", "plan", prov, ch)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool.Run(ctx)
	if len(ch) == 0 {
		t.Fatal("expected whisper")
	}
	w := <-ch
	if w.Salience < 0.9 {
		t.Errorf("expected salience >= 0.9 (provider path), got %f", w.Salience)
	}
}

// ── DefaultSpawner ──────────────────────────────────────────────────────────

func TestDefaultSpawner_NilProvider(t *testing.T) {
	// 不崩溃、不超时即通过
	ch := make(chan protocol.MemoryWhisper, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	DefaultSpawner(ctx, "some goal", "plan", nil, ch)
}

func TestDefaultSpawner_WithProvider(t *testing.T) {
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "spawner result"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	DefaultSpawner(ctx, "optimize", "plan", prov, ch)
	if len(ch) == 0 {
		t.Error("expected whisper from DefaultSpawner")
	}
}
