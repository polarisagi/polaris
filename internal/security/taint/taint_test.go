package taint

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestTaintedString(t *testing.T) {
	source := TaintSource{
		Module:           "test",
		EntityID:         "1",
		OriginTaintLevel: types.TaintHigh,
	}
	ts := NewTaintedString("malicious input", source, "user_input")

	if ts.UnsafeContent() != "malicious input" {
		t.Errorf("expected malicious input, got %s", ts.UnsafeContent())
	}
	if ts.Level() != types.TaintHigh {
		t.Errorf("expected TaintHigh, got %v", ts.Level())
	}
}

func TestSanitizeBySchema(t *testing.T) {
	ts := NewTaintedString("test", TaintSource{OriginTaintLevel: types.TaintHigh}, "")

	// 无 schema，应当失败
	tsFail, err := SanitizeBySchema(ts, false)
	if err == nil {
		t.Error("expected error for SanitizeBySchema without strict schema")
	}
	if tsFail.Level() != types.TaintHigh {
		t.Errorf("expected TaintHigh after failed sanitization, got %v", tsFail.Level())
	}

	// 有 schema，TaintHigh(3) -> TaintMedium(2)
	tsSuccess, err := SanitizeBySchema(ts, true)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if tsSuccess.Level() != types.TaintMedium {
		t.Errorf("expected TaintMedium, got %v", tsSuccess.Level())
	}

	// 继续降级 TaintMedium(2) -> TaintLow(1)
	tsLow, _ := SanitizeBySchema(tsSuccess, true)
	if tsLow.Level() != types.TaintLow {
		t.Errorf("expected TaintLow, got %v", tsLow.Level())
	}

	// 此时可以注入 SafeString
	safeStr, err := SanitizeToSafe(tsLow)
	if err != nil {
		t.Errorf("expected safe string generation, got err: %v", err)
	}
	if safeStr.Content() != "test" {
		t.Errorf("expected test, got %s", safeStr.Content())
	}
}

func TestSanitizeBySummarization(t *testing.T) {
	ts := NewTaintedString("test", TaintSource{OriginTaintLevel: types.TaintHigh}, "")

	// TaintHigh -> TaintMedium
	tsSumm := SanitizeBySummarization(ts)
	if tsSumm.Level() != types.TaintMedium {
		t.Errorf("expected TaintMedium, got %v", tsSumm.Level())
	}

	// TaintMedium -> 保持 TaintMedium (硬地板)
	tsSumm2 := SanitizeBySummarization(tsSumm)
	if tsSumm2.Level() != types.TaintMedium {
		t.Errorf("expected TaintMedium floor, got %v", tsSumm2.Level())
	}

	// 试图直接转化 SafeString 必须失败
	_, err := SanitizeToSafe(tsSumm2)
	if err == nil {
		t.Error("expected error when generating SafeString from TaintMedium")
	}
}

func TestSanitizeByUserReview(t *testing.T) {
	ts := NewTaintedString("test", TaintSource{OriginTaintLevel: types.TaintHigh}, "")

	tsReviewed := SanitizeByUserReview(ts, "admin-123")
	if tsReviewed.Level() != types.TaintUserReviewed {
		t.Errorf("expected TaintUserReviewed, got %v", tsReviewed.Level())
	}

	// TaintUserReviewed 可以转换为 SafeString
	safeStr, err := SanitizeToSafe(tsReviewed)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if safeStr.Content() != "test" {
		t.Errorf("expected test, got %s", safeStr.Content())
	}
}
