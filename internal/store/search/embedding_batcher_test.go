package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func makeEmbedFn(vecs [][]float32, err error) EmbedFn {
	return func(_ context.Context, texts []string, _ string) ([][]float32, error) {
		if err != nil {
			return nil, err
		}
		result := make([][]float32, len(texts))
		for i := range texts {
			if i < len(vecs) {
				result[i] = vecs[i]
			}
		}
		return result, nil
	}
}

func TestNewEmbeddingBatcher_Defaults(t *testing.T) {
	b := NewEmbeddingBatcher(0, 0, nil)
	if b.batchWindow != 10*time.Millisecond {
		t.Errorf("expected 10ms default batchWindow, got %v", b.batchWindow)
	}
	if b.maxBatchSize != 100 {
		t.Errorf("expected 100 default maxBatchSize, got %d", b.maxBatchSize)
	}
}

func TestFlushBatch_NilEmbedFn(t *testing.T) {
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, nil)
	_, err := b.flushBatch(context.Background(), []string{"hello"}, "text-emb-3")
	if err == nil {
		t.Fatal("expected error for nil embedFn")
	}
	var pe *apperr.Error
	if e, ok := err.(*apperr.Error); ok {
		pe = e
	}
	if pe == nil || pe.Code != apperr.CodeInternal {
		t.Errorf("expected CodeInternal, got: %v", err)
	}
}

func TestFlushBatch_Success(t *testing.T) {
	vecs := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, makeEmbedFn(vecs, nil))

	results, err := b.flushBatch(context.Background(), []string{"hello", "world"}, "text-emb-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Vector[0] != 0.1 || results[1].Vector[0] != 0.3 {
		t.Errorf("wrong vectors: %v", results)
	}
}

func TestFlushBatch_APIError(t *testing.T) {
	apiErr := apperr.New(apperr.CodeInternal, "rate limit")
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, makeEmbedFn(nil, apiErr))

	_, err := b.flushBatch(context.Background(), []string{"text"}, "text-emb-3")
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	var pe *apperr.Error
	if e, ok := err.(*apperr.Error); ok {
		pe = e
	}
	if pe == nil || pe.Code != apperr.CodeInternal {
		t.Errorf("expected CodeInternal, got: %v", err)
	}
	if pe.Cause == nil || !strings.Contains(pe.Cause.Error(), "rate limit") {
		t.Errorf("expected wrapped cause, got: %v", pe.Cause)
	}
}

func TestEmbed_LargeDirectFlush(t *testing.T) {
	vecs := make([][]float32, 100)
	for i := range vecs {
		vecs[i] = []float32{float32(i)}
	}
	b := NewEmbeddingBatcher(10*time.Millisecond, 5, makeEmbedFn(vecs, nil))

	texts := make([]string, 5) // len=5 >= maxBatchSize(5) → direct flush
	for i := range texts {
		texts[i] = "text"
	}
	results, err := b.Embed(context.Background(), texts, "text-emb-3", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("expected 5 results, got %d", len(results))
	}
}

// TestEmbed_HighNotBlockedByInflightLow 复现 2026-09-25 实测（ADR-0099）：后台 Low 批
// 在下游执行期间（100 条约 14s），交互 High 请求被串行 flush 堵在其后，稳定 30s 超时。
func TestEmbed_HighNotBlockedByInflightLow(t *testing.T) {
	lowStarted := make(chan struct{})
	releaseLow := make(chan struct{})
	fn := func(_ context.Context, texts []string, _ string) ([][]float32, error) {
		if strings.HasPrefix(texts[0], "bg") {
			close(lowStarted)
			<-releaseLow
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}
	b := NewEmbeddingBatcher(5*time.Millisecond, 100, fn)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { close(releaseLow); cancel(); b.Stop() }()
	b.Start(ctx)

	go func() { _, _ = b.Embed(ctx, []string{"bg-1"}, "m", PriorityLow) }()
	<-lowStarted

	hctx, hcancel := context.WithTimeout(ctx, time.Second)
	defer hcancel()
	res, err := b.Embed(hctx, []string{"user query"}, "m", PriorityHigh)
	if err != nil || len(res) != 1 || res[0].Error != nil {
		t.Fatalf("High 请求不得被在途 Low 批阻塞: err=%v res=%+v", err, res)
	}
}

// TestEmbed_CallTimeoutUnfreezesLane 后端挂起只让该批失败，不冻结整条通道。
func TestEmbed_CallTimeoutUnfreezesLane(t *testing.T) {
	calls := 0
	fn := func(ctx context.Context, texts []string, _ string) ([][]float32, error) {
		calls++
		if calls == 1 {
			<-ctx.Done() // 模拟后端挂起
			return nil, ctx.Err()
		}
		return [][]float32{{1}}, nil
	}
	b := NewEmbeddingBatcher(5*time.Millisecond, 100, fn).WithLaneLimits(0, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); b.Stop() }()
	b.Start(ctx)

	if res, _ := b.Embed(ctx, []string{"a"}, "m", PriorityHigh); res[0].Error == nil {
		t.Fatal("首批应因下游超时失败")
	}
	res, err := b.Embed(ctx, []string{"b"}, "m", PriorityHigh)
	if err != nil || res[0].Error != nil {
		t.Fatalf("超时后通道必须恢复可用: err=%v res=%+v", err, res)
	}
}

// TestEmbed_MultiTextSingleCycle 多文本一次入队、同批返回，而非逐条串行等待。
func TestEmbed_MultiTextSingleCycle(t *testing.T) {
	calls := 0
	fn := func(_ context.Context, texts []string, _ string) ([][]float32, error) {
		calls++
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{float32(i)}
		}
		return out, nil
	}
	b := NewEmbeddingBatcher(20*time.Millisecond, 100, fn)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); b.Stop() }()
	b.Start(ctx)

	res, err := b.Embed(ctx, []string{"x", "y", "z"}, "m", PriorityHigh)
	if err != nil || len(res) != 3 {
		t.Fatalf("err=%v len=%d", err, len(res))
	}
	if calls != 1 {
		t.Fatalf("3 条文本应在同一批内完成，实际下游调用 %d 次", calls)
	}
}
