package reflexion

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// MockEpisodicMemory for testing
type MockEpisodicMemory struct {
	QueryFunc func(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error)
}

func (m *MockEpisodicMemory) Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error {
	return nil
}
func (m *MockEpisodicMemory) Query(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
	if m.QueryFunc != nil {
		return m.QueryFunc(ctx, query)
	}
	return nil, nil
}
func (m *MockEpisodicMemory) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	return 0, nil
}

// MockProvider for testing
type MockProvider struct {
	InferFunc func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)
}

func (m *MockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *MockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *MockProvider) ModelID() string                      { return "mock" }

func (m *MockProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if m.InferFunc != nil {
		return m.InferFunc(ctx, messages, opts...)
	}
	return &types.ProviderResponse{}, nil
}
func (m *MockProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}

// MockReflectionMemory for testing
type MockReflectionMemory struct {
	AppendFunc func(ctx context.Context, entry types.ReflectionEntry) error
}

func (m *MockReflectionMemory) AppendReflection(ctx context.Context, entry types.ReflectionEntry) error {
	if m.AppendFunc != nil {
		return m.AppendFunc(ctx, entry)
	}
	return nil
}
func (m *MockReflectionMemory) QueryReflections(ctx context.Context, query types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	return nil, nil
}
func (m *MockReflectionMemory) GetBySession(ctx context.Context, sessionID string) ([]types.ReflectionEntry, error) {
	return nil, nil
}

func TestReflectionWorker(t *testing.T) {
	ctx := context.Background()

	t.Run("Whitelisted_TaskType", func(t *testing.T) {
		rw := NewReflectionWorker(nil, nil, nil)
		if !rw.isWhitelisted("coding") {
			t.Errorf("Expected coding to be whitelisted")
		}
		if rw.isWhitelisted("unknown") {
			t.Errorf("Expected unknown not to be whitelisted")
		}
	})

	t.Run("Config_Overrides", func(t *testing.T) {
		cfg := ReflectionConfig{
			TaskTypeWhitelist: []string{"custom"},
			MinReplanCount:    5,
		}
		rw := NewReflectionWorkerWithConfig(nil, nil, nil, cfg)
		if rw.isWhitelisted("coding") {
			t.Errorf("Expected coding to not be whitelisted")
		}
		if !rw.isWhitelisted("custom") {
			t.Errorf("Expected custom to be whitelisted")
		}
	})

	t.Run("Consolidate_Skip_ReplanCount", func(t *testing.T) {
		rw := NewReflectionWorkerWithConfig(nil, nil, nil, ReflectionConfig{
			TaskTypeWhitelist: []string{"test"},
			MinReplanCount:    3,
		})
		err := rw.ConsolidateReflections(ctx, "task1", "other", 2, true)
		if err != nil {
			t.Errorf("Expected nil error")
		}
	})

	t.Run("Consolidate_Empty_Events", func(t *testing.T) {
		mem := &MockEpisodicMemory{
			QueryFunc: func(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
				return nil, nil
			},
		}
		rw := NewReflectionWorker(mem, nil, nil)
		err := rw.ConsolidateReflections(ctx, "task1", "coding", 0, true)
		if err != nil {
			t.Errorf("Expected nil error")
		}
	})

	t.Run("Consolidate_Success", func(t *testing.T) {
		mem := &MockEpisodicMemory{
			QueryFunc: func(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
				return []types.ScoredEvent{
					{Event: types.Event{ID: "e1", Type: "action", Payload: []byte("test")}},
				}, nil
			},
		}
		prov := &MockProvider{
			InferFunc: func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
				return &types.ProviderResponse{
					Content: `{"reflection_type": "success_pattern", "content": "test content"}`,
				}, nil
			},
		}

		appended := false
		refMem := &MockReflectionMemory{
			AppendFunc: func(ctx context.Context, entry types.ReflectionEntry) error {
				appended = true
				if entry.Strategy != "success_pattern" {
					t.Errorf("Expected strategy success_pattern, got %s", entry.Strategy)
				}
				if entry.Decision != "test content" {
					t.Errorf("Expected decision test content, got %s", entry.Decision)
				}
				return nil
			},
		}

		rw := NewReflectionWorker(mem, prov, refMem)
		err := rw.ConsolidateReflections(ctx, "task1", "coding", 0, true)
		if err != nil {
			t.Errorf("ConsolidateReflections failed: %v", err)
		}
		if !appended {
			t.Errorf("Expected AppendReflection to be called")
		}
	})

	t.Run("Consolidate_Malformed_JSON", func(t *testing.T) {
		mem := &MockEpisodicMemory{
			QueryFunc: func(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
				return []types.ScoredEvent{
					{Event: types.Event{ID: "e1", Type: "action", Payload: []byte("test")}},
				}, nil
			},
		}
		prov := &MockProvider{
			InferFunc: func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
				return &types.ProviderResponse{
					Content: `malformed json`,
				}, nil
			},
		}

		rw := NewReflectionWorker(mem, prov, nil)
		err := rw.ConsolidateReflections(ctx, "task1", "coding", 0, true)
		if err == nil {
			t.Errorf("Expected JSON parse error")
		}
	})
}
