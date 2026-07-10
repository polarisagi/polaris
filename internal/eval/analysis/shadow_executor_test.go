package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	_ "modernc.org/sqlite"
)

// mockProvider 模拟 LLM 推理
type mockProvider struct {
	inferResp     *types.ProviderResponse
	inferErr      error
	judgeResp     *types.ProviderResponse
	judgeErr      error
	callCount     int
	lastShadowMsg []types.Message // 最近一次影子推理（非 judge 调用）收到的消息，供断言 system 覆盖是否生效
}

func (m *mockProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	m.callCount++
	if len(msgs) > 0 && msgs[0].Role == "user" && strings.HasPrefix(msgs[0].Content, "你是一个严格的对比评判器") {
		if m.judgeErr != nil {
			return nil, m.judgeErr
		}
		return m.judgeResp, nil
	}
	m.lastShadowMsg = msgs
	if m.inferErr != nil {
		return nil, m.inferErr
	}
	return m.inferResp, nil
}

func (m *mockProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (m *mockProvider) Capabilities() types.ProviderCapabilities { return types.ProviderCapabilities{} }
func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }
func (m *mockProvider) ModelID() string                          { return "mock" }

// mockCache 模拟工具调用的 032_mock_response_cache
type mockCache struct {
	repo.MockResponseCache
	responses map[string]*protocol.MockResponse
	hitCount  int
	missCount int
}

func (m *mockCache) GetMockResponse(ctx context.Context, opHash string) (*protocol.MockResponse, error) {
	if resp, ok := m.responses[opHash]; ok {
		m.hitCount++
		return resp, nil
	}
	m.missCount++
	return nil, apperr.New(apperr.CodeNotFound, "not found")
}

// mockStaging 模拟 Gate 1 的 ConfirmShadow 与 Rollback
type mockStaging struct {
	optimizer.StagingPipeline
	confirmCount  int
	rollbackCount int
}

func (m *mockStaging) ConfirmShadow(ctx context.Context, version string) error {
	m.confirmCount++
	return nil
}
func (m *mockStaging) Rollback(ctx context.Context, version string, reason string) error {
	m.rollbackCount++
	return nil
}

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE events(offset INTEGER PRIMARY KEY, type TEXT, payload BLOB)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	return db
}

func TestShadowExecutor_RunReplayBatch(t *testing.T) {
	t.Run("缓存未命中跳过并计数", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "test"}}}
		payload, _ := json.Marshal(map[string]any{
			"request": req,
		})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)

		provider := &mockProvider{
			inferResp: &types.ProviderResponse{
				ToolCalls: []types.InferToolCall{{Name: "test_tool", Input: []byte("{}")}},
			},
		}

		cache := &mockCache{
			responses: make(map[string]*protocol.MockResponse),
		}
		staging := &mockStaging{}

		exec := NewShadowExecutor(db, provider, cache, nil, staging)
		exec.thresholds.M12Eval.ShadowMinSamples = 0
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		err := exec.RunReplayBatch(context.Background(), "v2", "", nil)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}

		if cache.missCount != 1 {
			t.Fatalf("expected 1 miss, got %d", cache.missCount)
		}
	})

	t.Run("采样游标幂等_批次间不重复回放", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "test"}}}
		payload, _ := json.Marshal(map[string]any{
			"request":  req,
			"response": &types.InferResponse{Content: "baseline output"},
		})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)

		provider := &mockProvider{
			inferResp: &types.ProviderResponse{Content: "shadow output"},
			judgeResp: &types.ProviderResponse{Content: `{"passed":true,"reason":"ok"}`},
		}
		staging := &mockStaging{}

		exec := NewShadowExecutor(db, provider, &mockCache{}, nil, staging)
		exec.thresholds.M12Eval.ShadowMinSamples = 0
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		if err := exec.RunReplayBatch(context.Background(), "v2", "", nil); err != nil {
			t.Fatalf("first batch: %v", err)
		}
		firstCalls := provider.callCount
		if firstCalls == 0 {
			t.Fatalf("expected first batch to infer, got 0 calls")
		}

		// 第二批：无新事件，游标已推进，不得重复回放
		if err := exec.RunReplayBatch(context.Background(), "v2", "", nil); err != nil {
			t.Fatalf("second batch: %v", err)
		}
		if provider.callCount != firstCalls {
			t.Fatalf("expected no replay on second batch, calls %d -> %d", firstCalls, provider.callCount)
		}
	})

	t.Run("payload缺request字段_跳过不崩溃", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		payload, _ := json.Marshal(map[string]any{"response": &types.InferResponse{Content: "x"}})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)

		exec := NewShadowExecutor(db, &mockProvider{}, &mockCache{}, nil, &mockStaging{})
		exec.thresholds.M12Eval.ShadowMinSamples = 0
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		if err := exec.RunReplayBatch(context.Background(), "v2", "", nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("门控信号阈值判定_通过", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "test"}}}
		payload, _ := json.Marshal(map[string]any{
			"request":  req,
			"response": &types.InferResponse{Content: "baseline output"},
		})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(2, 'llm_call', ?)`, payload)

		provider := &mockProvider{
			inferResp: &types.ProviderResponse{Content: "shadow output"},
			judgeResp: &types.ProviderResponse{Content: `{"passed":true,"reason":"ok"}`},
		}
		cache := &mockCache{}
		staging := &mockStaging{}

		exec := NewShadowExecutor(db, provider, cache, nil, staging)
		exec.thresholds.M12Eval.ShadowMinSamples = 2
		exec.thresholds.M12Eval.ShadowPassRateThreshold = 0.95
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		err := exec.RunReplayBatch(context.Background(), "v2", "", nil)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}

		if staging.confirmCount != 1 {
			t.Fatalf("expected 1 confirm, got %d", staging.confirmCount)
		}
		if staging.rollbackCount != 0 {
			t.Fatalf("expected 0 rollback, got %d", staging.rollbackCount)
		}
	})

	t.Run("门控信号阈值判定_回滚", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "test"}}}
		payload, _ := json.Marshal(map[string]any{
			"request":  req,
			"response": &types.InferResponse{Content: "baseline output"},
		})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(2, 'llm_call', ?)`, payload)

		provider := &mockProvider{
			inferResp: &types.ProviderResponse{Content: "shadow output"},
			judgeResp: &types.ProviderResponse{Content: `{"passed":false,"reason":"fail"}`},
		}
		cache := &mockCache{}
		staging := &mockStaging{}

		exec := NewShadowExecutor(db, provider, cache, nil, staging)
		exec.thresholds.M12Eval.ShadowMinSamples = 2
		exec.thresholds.M12Eval.ShadowPassRateThreshold = 0.95
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		err := exec.RunReplayBatch(context.Background(), "v2", "", nil)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}

		if staging.confirmCount != 0 {
			t.Fatalf("expected 0 confirm, got %d", staging.confirmCount)
		}
		if staging.rollbackCount != 1 {
			t.Fatalf("expected 1 rollback, got %d", staging.rollbackCount)
		}
	})

	t.Run("systemPromptOverride替换首条system消息", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{
			{Role: "system", Content: "old system prompt"},
			{Role: "user", Content: "hello"},
		}}
		payload, _ := json.Marshal(map[string]any{"request": req})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)

		provider := &mockProvider{inferResp: &types.ProviderResponse{Content: "shadow output"}}
		exec := NewShadowExecutor(db, provider, &mockCache{}, nil, &mockStaging{})
		exec.thresholds.M12Eval.ShadowMinSamples = 0
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		if err := exec.RunReplayBatch(context.Background(), "v2", "new candidate prompt", nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}

		if len(provider.lastShadowMsg) != 2 {
			t.Fatalf("expected 2 messages (system+user), got %d", len(provider.lastShadowMsg))
		}
		if provider.lastShadowMsg[0].Role != "system" || provider.lastShadowMsg[0].Content != "new candidate prompt" {
			t.Errorf("expected system message replaced with override, got %+v", provider.lastShadowMsg[0])
		}
		if provider.lastShadowMsg[1].Content != "hello" {
			t.Errorf("expected non-system messages preserved, got %+v", provider.lastShadowMsg[1])
		}
	})

	t.Run("systemPromptOverride在无system消息时前插", func(t *testing.T) {
		db := setupTestDB(t)
		defer db.Close()

		req := &types.InferRequest{Messages: []types.Message{{Role: "user", Content: "hello"}}}
		payload, _ := json.Marshal(map[string]any{"request": req})
		db.Exec(`INSERT INTO events(offset, type, payload) VALUES(1, 'llm_call', ?)`, payload)

		provider := &mockProvider{inferResp: &types.ProviderResponse{Content: "shadow output"}}
		exec := NewShadowExecutor(db, provider, &mockCache{}, nil, &mockStaging{})
		exec.thresholds.M12Eval.ShadowMinSamples = 0
		exec.thresholds.M12Eval.ShadowSampleRate = 1.0

		if err := exec.RunReplayBatch(context.Background(), "v2", "prepended prompt", nil); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}

		if len(provider.lastShadowMsg) != 2 {
			t.Fatalf("expected 2 messages (prepended system+user), got %d", len(provider.lastShadowMsg))
		}
		if provider.lastShadowMsg[0].Role != "system" || provider.lastShadowMsg[0].Content != "prepended prompt" {
			t.Errorf("expected system message prepended, got %+v", provider.lastShadowMsg[0])
		}
	})
}
