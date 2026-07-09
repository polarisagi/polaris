package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakePRMProvider 是 protocol.LocalProvider 的测试替身，只有 Infer 行为可配置。
type fakePRMProvider struct {
	content string
	err     error
	delay   time.Duration
}

func (f *fakePRMProvider) Infer(ctx context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &types.ProviderResponse{Content: f.content}, nil
}
func (f *fakePRMProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (f *fakePRMProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (f *fakePRMProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (f *fakePRMProvider) ModelID() string                      { return "fake-prm" }
func (f *fakePRMProvider) LoadModel(context.Context, string, protocol.LocalModelOptions) error {
	return nil
}
func (f *fakePRMProvider) UnloadModel(context.Context) error  { return nil }
func (f *fakePRMProvider) EvictKVCache(context.Context) error { return nil }
func (f *fakePRMProvider) LocalStatus(context.Context) (protocol.LocalModelStatus, error) {
	return protocol.LocalModelStatus{}, nil
}
func (f *fakePRMProvider) Probe(context.Context) (protocol.LocalProbeResult, error) {
	return protocol.LocalProbeResult{}, nil
}

var _ protocol.LocalProvider = (*fakePRMProvider)(nil)

func baseScorer() *stepScorer {
	return &stepScorer{
		toolSuccessWeight: 0.4,
		schemaCheckWeight: 0.3,
		latencyWeight:     0.2,
		tokenEfficiencyWt: 0.1,
	}
}

func TestScoreWithPRM_NilPRM_FallsBackToStatic(t *testing.T) {
	s := baseScorer() // prm == nil
	c := stepCtx{ToolResult: true, SchemaPassed: true}
	if got := s.scoreWithPRM(context.Background(), c, "irrelevant"); got != s.score(c) {
		t.Errorf("expected pure static score %f, got %f", s.score(c), got)
	}
}

func TestScoreWithPRM_PositiveSignal_FusesUpward(t *testing.T) {
	s := baseScorer()
	s.prm = &fakePRMProvider{content: "+1"}
	c := stepCtx{ToolResult: false, SchemaPassed: true} // static score < 1.0
	static := s.score(c)
	fused := s.scoreWithPRM(context.Background(), c, "step failed but content ok")
	if fused <= static {
		t.Errorf("expected PRM +1 signal to raise fused score above static baseline: static=%f fused=%f", static, fused)
	}
	want := (1-prmStepWeight)*static + prmStepWeight*1.0
	if diff := fused - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("fused score mismatch: got %f want %f", fused, want)
	}
}

func TestScoreWithPRM_NegativeSignal_FusesDownward(t *testing.T) {
	s := baseScorer()
	s.prm = &fakePRMProvider{content: "-1"}
	c := stepCtx{ToolResult: true, SchemaPassed: true} // static score == 1.0
	static := s.score(c)
	fused := s.scoreWithPRM(context.Background(), c, "tool succeeded but semantically wrong")
	if fused >= static {
		t.Errorf("expected PRM -1 signal to lower fused score below static baseline: static=%f fused=%f", static, fused)
	}
}

func TestScoreWithPRM_Timeout_DegradesToStatic(t *testing.T) {
	s := baseScorer()
	s.prm = &fakePRMProvider{content: "+1", delay: 500 * time.Millisecond} // > prmTimeout
	c := stepCtx{ToolResult: true, SchemaPassed: true}
	static := s.score(c)
	start := time.Now()
	fused := s.scoreWithPRM(context.Background(), c, "slow step")
	elapsed := time.Since(start)
	if fused != static {
		t.Errorf("expected degrade to static score on timeout: static=%f fused=%f", static, fused)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("expected hard ~100ms timeout to bound latency, took %v", elapsed)
	}
}

func TestScoreWithPRM_InferError_DegradesToStatic(t *testing.T) {
	s := baseScorer()
	s.prm = &fakePRMProvider{err: errors.New("oom")}
	c := stepCtx{ToolResult: true, SchemaPassed: true}
	static := s.score(c)
	if fused := s.scoreWithPRM(context.Background(), c, "oom step"); fused != static {
		t.Errorf("expected degrade to static score on infer error: static=%f fused=%f", static, fused)
	}
}

func TestScoreWithPRM_NonConformingOutput_DegradesToStatic(t *testing.T) {
	s := baseScorer()
	s.prm = &fakePRMProvider{content: "definitely not a valid token"}
	c := stepCtx{ToolResult: true, SchemaPassed: true}
	static := s.score(c)
	if fused := s.scoreWithPRM(context.Background(), c, "garbled output"); fused != static {
		t.Errorf("expected degrade to static score on non-conforming PRM output: static=%f fused=%f", static, fused)
	}
}

func TestNewStepScorer_Tier0OrRemoteProvider_NoPRM(t *testing.T) {
	// 非 LocalProvider（如远程 Provider）：不启用 PRM，等价于 newDefaultStepScorer()。
	s := newStepScorer(nil)
	if s.prm != nil {
		t.Error("expected prm to be nil for non-LocalProvider (nil) provider")
	}
}

func TestSummarizeStepForPRM_Truncates(t *testing.T) {
	longOutput := make([]byte, prmSummaryMaxLen+50)
	for i := range longOutput {
		longOutput[i] = 'x'
	}
	res := &types.ToolResult{Success: true, Output: longOutput}
	summary := summarizeStepForPRM("some_tool", true, res, nil)
	if len(summary) > prmSummaryMaxLen+40 { // 加上 "tool=...status=...detail=" 前缀余量
		t.Errorf("expected truncated summary, got length %d", len(summary))
	}
}
