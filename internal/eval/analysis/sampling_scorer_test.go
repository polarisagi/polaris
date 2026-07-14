package analysis

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestJudgeReplyQuality_Success 2026-07-14：验证正常解析路径，直接取模型
// 返回的纯数字文本作为分数。复用 shadow_executor_test.go 定义的 mockProvider
// （同包，Infer 对非 judge-前缀的 user 消息直接返回 inferResp）。
func TestJudgeReplyQuality_Success(t *testing.T) {
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "0.85"}}
	score, err := judgeReplyQuality(context.Background(), p, "问题", "回复")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 0.85 {
		t.Errorf("expected score=0.85, got %v", score)
	}
}

func TestJudgeReplyQuality_ClampsAboveOne(t *testing.T) {
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "1.50"}}
	score, err := judgeReplyQuality(context.Background(), p, "q", "r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 1.0 {
		t.Errorf("expected score clamped to 1.0, got %v", score)
	}
}

func TestJudgeReplyQuality_ClampsBelowZero(t *testing.T) {
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "-0.30"}}
	score, err := judgeReplyQuality(context.Background(), p, "q", "r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score clamped to 0, got %v", score)
	}
}

// TestJudgeReplyQuality_ToleratesExtraText 模型偶尔附带多余文字（如"分数：0.7分"），
// extractLeadingFloat 应容错截取。
func TestJudgeReplyQuality_ToleratesExtraText(t *testing.T) {
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "  0.7 分\n"}}
	score, err := judgeReplyQuality(context.Background(), p, "q", "r")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 0.7 {
		t.Errorf("expected score=0.7, got %v", score)
	}
}

func TestJudgeReplyQuality_UnparsableGarbage(t *testing.T) {
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "无法评分"}}
	if _, err := judgeReplyQuality(context.Background(), p, "q", "r"); err == nil {
		t.Error("expected parse error for non-numeric judge output")
	}
}

func TestJudgeReplyQuality_InferErrorPropagates(t *testing.T) {
	p := &mockProvider{inferErr: context.DeadlineExceeded}
	if _, err := judgeReplyQuality(context.Background(), p, "q", "r"); err == nil {
		t.Error("expected wrapped infer error")
	}
}

func TestExtractLeadingFloat(t *testing.T) {
	cases := map[string]string{
		"0.85":        "0.85",
		"  0.7 分":     "0.7",
		"分数：0.42":     "0.42",
		"1.0":         "1.0",
		"no digits":   "no digits", // 无数字：原样返回，交给 ParseFloat 报错
		"score=0.99!": "0.99",
		"-0.30":       "-0.30",
	}
	for in, want := range cases {
		if got := extractLeadingFloat(in); got != want {
			t.Errorf("extractLeadingFloat(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMaybeSampleAndScore_SkipsWhenProviderNil 确定性跳过路径：provider=nil
// 不应记录任何样本（窗口保持全零，CheckDegradation 报"样本不足"）。
func TestMaybeSampleAndScore_SkipsWhenProviderNil(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	m.MaybeSampleAndScore(nil, "session-1", "q", "r")
	if m.windowHead != 0 || m.windowFilled {
		t.Error("expected no sample recorded when provider is nil")
	}
}

// TestMaybeSampleAndScore_SkipsWhenResponseEmpty 确定性跳过路径：response 为
// 空字符串（无有效回复可评）不应记录样本。
func TestMaybeSampleAndScore_SkipsWhenResponseEmpty(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	p := &mockProvider{inferResp: &types.ProviderResponse{Content: "0.9"}}
	m.MaybeSampleAndScore(p, "session-1", "q", "   ")
	if m.windowHead != 0 || m.windowFilled {
		t.Error("expected no sample recorded when response is blank")
	}
}
