package planner

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

// ── mock WorkspaceStager（WP-9：workerEngineA 落盘依赖注入验证）──────────────

// mockWorkspaceStager 用测试专用临时目录模拟 *vfs.WorkspaceManager.StageEphemeralFile，
// 记录每次落盘/清理调用次数，验证 workerEngineA 不再直连裸 os.MkdirTemp/os.WriteFile，
// 而是完全通过注入的 WorkspaceStager 接口落盘并在 defer cleanup() 时归还。
type mockWorkspaceStager struct {
	mu          sync.Mutex
	staged      []string
	cleanupCall int
}

func (m *mockWorkspaceStager) StageEphemeralFile(namespace, filename string, data []byte) (string, func(), error) {
	dir := os.TempDir() // 测试替身允许直接用系统临时目录；生产实现见 vfs.WorkspaceManager
	path := filepath.Join(dir, namespace+"-"+filename)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	m.staged = append(m.staged, namespace+"/"+filename)
	m.mu.Unlock()
	cleanup := func() {
		m.mu.Lock()
		m.cleanupCall++
		m.mu.Unlock()
		_ = os.Remove(path)
	}
	return path, cleanup, nil
}

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
	pool := NewPlannerPool("fix bug", "code_act", nil, ch, nil)
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
	pool := NewPlannerPool("goal", "plan", nil, nil, nil)
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
	pool := NewPlannerPool("plan something", "plan", nil, ch, nil)
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
	pool := NewPlannerPool("refactor", "plan", prov, ch, nil)
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
	pool := NewPlannerPool("goal", "plan", prov, ch, nil)
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

// ── PlannerPool 端到端（原 DefaultSpawner 覆盖范围）─────────────────────────
//
// 2026-08-08：DefaultSpawner 是仅测试可达的便捷包装（其自身注释即写明"生产环境
// 的真实 spawner 由 cmd/polaris/boot_agent.go 构造"），已删除。这两个用例改为
// 直接走 NewPlannerPool + Run，与生产注入路径一致。

func TestPlannerPool_NilProvider(t *testing.T) {
	// 不崩溃、不超时即通过
	ch := make(chan protocol.MemoryWhisper, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	NewPlannerPool("some goal", "plan", nil, ch, nil).Run(ctx)
}

func TestPlannerPool_WithProvider(t *testing.T) {
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "spawner result"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	NewPlannerPool("optimize", "plan", prov, ch, nil).Run(ctx)
	if len(ch) == 0 {
		t.Error("expected whisper from PlannerPool")
	}
}

// ── workerEngineA / WorkspaceStager 注入（WP-9）───────────────────────────────

// TestPlannerPool_WorkerEngineA_NilWorkspaceSkips 验证 workerEngineA 在未注入
// WorkspaceStager 时直接跳过编译评分（不回退到裸 os.MkdirTemp/os.WriteFile），
// 因此 code_act 三个 worker 均不产出结果，whisperChan 保持空。
func TestPlannerPool_WorkerEngineA_NilWorkspaceSkips(t *testing.T) {
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "package main"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	pool := NewPlannerPool("fix bug", "code_act", prov, ch, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool.Run(ctx)
	if len(ch) != 0 {
		t.Errorf("expected no whisper when WorkspaceStager not injected, got %d", len(ch))
	}
}

// TestPlannerPool_WorkerEngineA_WithWorkspace_StagesAndCleansUp 验证注入
// WorkspaceStager 后 workerEngineA 通过接口落盘（而非直连文件系统），且每次
// 落盘都在 defer cleanup() 时被正确回收——3 个并发 worker 各自落盘一次、
// 各自清理一次。
func TestPlannerPool_WorkerEngineA_WithWorkspace_StagesAndCleansUp(t *testing.T) {
	prov := &mockProvider{resp: &types.ProviderResponse{Content: "package main"}}
	ch := make(chan protocol.MemoryWhisper, 10)
	pool := NewPlannerPool("fix bug", "code_act", prov, ch, nil)
	ws := &mockWorkspaceStager{}
	pool.SetWorkspace(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Run(ctx)

	if len(ch) == 0 {
		t.Fatal("expected whisper when WorkspaceStager injected")
	}

	ws.mu.Lock()
	staged := len(ws.staged)
	cleanups := ws.cleanupCall
	ws.mu.Unlock()

	if staged != 3 {
		t.Errorf("expected 3 staged files (1 per code_act worker), got %d", staged)
	}
	if cleanups != staged {
		t.Errorf("expected cleanup() called once per staged file, got %d cleanups for %d staged", cleanups, staged)
	}
}
