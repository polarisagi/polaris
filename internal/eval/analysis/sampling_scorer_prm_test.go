package analysis

import (
	"context"
	"sync"
	"testing"
	"time"

	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakePRMAdapter struct {
	mu      sync.Mutex
	batches [][]llmadapter.TrainingSample
}

func (f *fakePRMAdapter) Train(ctx context.Context, samples []llmadapter.TrainingSample) (*llmadapter.TrainingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, samples)
	return &llmadapter.TrainingResult{JobID: "job"}, nil
}

func (f *fakePRMAdapter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func (f *fakePRMAdapter) lastBatch() []llmadapter.TrainingSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	return f.batches[len(f.batches)-1]
}

// TestMaybeSampleAndScore_FeedsPRMCollector 验证 M12 §9 生产采样打分成功时会
// 把 (query, response, score) 作为 PRM TrainingSample 转发给采集器。
// MaybeSampleAndScore 内部按 1% 概率采样（真实生产语义），测试通过重复调用+
// 轮询等待的方式规避随机性导致的偶发不触发（期望调用数下 P(全部未命中)可忽略）。
func TestMaybeSampleAndScore_FeedsPRMCollector(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	fake := &fakePRMAdapter{}
	collector := llmadapter.NewTrainingSampleCollector("prm", fake, 1) // batchSize=1：一有样本立即触发
	m.InjectPRMCollector(collector)

	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "0.77"}}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fake.count() == 0 {
		m.MaybeSampleAndScore(p, "session-1", "问题内容", "回复内容")
		time.Sleep(time.Millisecond)
	}

	if fake.count() == 0 {
		t.Fatal("expected at least one PRM training sample recorded within deadline")
	}
	batch := fake.lastBatch()
	if len(batch) != 1 {
		t.Fatalf("expected batch of 1 sample, got %d", len(batch))
	}
	if batch[0].Prompt != "问题内容" || batch[0].Completion != "回复内容" || batch[0].Reward != 0.77 {
		t.Errorf("unexpected training sample: %+v", batch[0])
	}
}

// TestMaybeSampleAndScore_NilPRMCollector_NoPanic 未注入 PRM 采集器时（默认
// nil）仍应正常完成退化监控采样，不 panic。
func TestMaybeSampleAndScore_NilPRMCollector_NoPanic(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "0.5"}}
	for i := 0; i < 20; i++ {
		m.MaybeSampleAndScore(p, "s", "q", "r")
	}
	time.Sleep(50 * time.Millisecond) // 让可能触发的采样 goroutine 跑完
}
