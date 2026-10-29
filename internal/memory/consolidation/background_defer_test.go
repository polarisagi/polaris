package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory"
	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// deferredErr 模拟 InferenceRouter → safecall → InferRaw 逐层包裹后的推迟信号：
// 外层错误码已不是 ResourceExhausted，只能按链判别。
func deferredErr() error {
	return apperr.Wrap(apperr.CodeInternal, "failed to infer", apperr.Wrap(apperr.CodeProviderExhausted, "llm infer failed", routedDeferredErr()))
}

// routedDeferredErr 路由原样返回的形态：外层错误码即 ResourceExhausted，与 OOM 同码。
func routedDeferredErr() error {
	return apperr.Wrap(apperr.CodeResourceExhausted, "router deferred", protocol.ErrBackgroundDeferred)
}

func newDeferTestPipeline(summ memory.LLMSummarizer) *ConsolidationPipeline {
	store := testutil.NewMockStore()
	return NewConsolidationPipelineFull(memstore.NewEpisodicMem(store),
		memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{}), &mockSkillRegistry{}, summ, nil, nil, nil)
}

// TestExtract_PropagatesBackgroundDeferral 资源压力下的推迟必须上抛，交 outbox 择机重做，
// 不得降级为规则抽取落库（那会以降质结果把本条标记完成）。
func TestExtract_PropagatesBackgroundDeferral(t *testing.T) {
	events := scoredEventsWithPayload("s1", "see https://example.com/docs", 15)

	t.Run("llmExtract", func(t *testing.T) {
		pipe := newDeferTestPipeline(&mockSummarizer{err: deferredErr()})
		if _, _, err := pipe.extractEntitiesAndRelations(context.Background(), "s1", events); !errors.Is(err, protocol.ErrBackgroundDeferred) {
			t.Fatalf("推迟信号应上抛，got %v", err)
		}
	})
	t.Run("SharedEntityExtractor", func(t *testing.T) {
		pipe := newDeferTestPipeline(&mockSummarizer{})
		pipe.WithEntityExtractor(&fakeSharedExtractor{err: deferredErr()})
		if _, _, err := pipe.extractEntitiesAndRelations(context.Background(), "s1", events); !errors.Is(err, protocol.ErrBackgroundDeferred) {
			t.Fatalf("推迟信号应上抛，got %v", err)
		}
	})
	t.Run("executeStages", func(t *testing.T) {
		pipe := newDeferTestPipeline(&mockSummarizer{err: deferredErr()})
		if err := pipe.executeStages(context.Background(), "s1", events); !errors.Is(err, protocol.ErrBackgroundDeferred) {
			t.Fatalf("Stage 1 推迟不得按非阻断吞掉，got %v", err)
		}
	})
	t.Run("PerMessageExtractor", func(t *testing.T) {
		pe := NewPerMessageExtractor(newDeferTestPipeline(&mockSummarizer{err: deferredErr()}))
		payload, _ := json.Marshal(map[string]string{"session_id": "s1", "event_type": "observation", "content": "see https://example.com/docs"})
		if err := pe.HandleOutboxRecord(context.Background(), payload); !errors.Is(err, protocol.ErrBackgroundDeferred) {
			t.Fatalf("逐条提取的推迟信号应返回给 outbox，got %v", err)
		}
	})
}

type recordingOutbox struct{ writes []protocol.OutboxEntry }

func (r *recordingOutbox) Write(_ context.Context, e protocol.OutboxEntry) error {
	r.writes = append(r.writes, e)
	return nil
}

// TestRun_DeferralDoesNotScheduleOOMRetry 推迟由 outbox 自身择机重做本条；若再按 OOM
// 另投 memory_consolidate_retry，压力期间每个 tick 都会多出一条重试消息。
func TestRun_DeferralDoesNotScheduleOOMRetry(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	episodic := memstore.NewEpisodicMem(store)
	for range 15 {
		_ = episodic.Append(ctx, types.Event{ID: "e1", TaskID: "s1", Type: "tool_call",
			Payload: []byte("see https://example.com/docs"), CreatedAt: time.Now()}, types.TaintNone)
	}
	ob := &recordingOutbox{}
	pipe := NewConsolidationPipelineFull(episodic, memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{}),
		&mockSkillRegistry{}, &mockSummarizer{}, nil, nil, nil).WithOutbox(ob)
	// 外层与 OOM 同为 ResourceExhausted：只凭错误码会误判为 OOM 并另投重试。
	pipe.WithEntityExtractor(&fakeSharedExtractor{err: routedDeferredErr()})

	if err := pipe.Run(ctx, "s1"); !errors.Is(err, protocol.ErrBackgroundDeferred) {
		t.Fatalf("推迟应上抛给 outbox，got %v", err)
	}
	if len(ob.writes) != 0 {
		t.Fatalf("推迟不得另投 OOM 重试消息，got %d 条", len(ob.writes))
	}
}
