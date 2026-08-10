package store

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateEpisodicPayload_ProducesValidJSON 验证 preview 中包含引号/换行/反斜杠
// 等 JSON 特殊字符时，truncateEpisodicPayload 的返回值仍是合法 JSON（ADR-0094
// 决策六：结构化载体禁字符串直拼）。此前用 fmt.Sprintf(`"preview":%s`) 直拼原始
// 文本，遇到这些字符会产出语法损坏的 JSON，下游 Unmarshal 必然失败。
func TestTruncateEpisodicPayload_ProducesValidJSON(t *testing.T) {
	em := NewEpisodicMem(nil)

	raw := []byte(`{"nested":"value with "quotes" and\nnewlines and \backslashes\"}`)
	out := em.truncateEpisodicPayload("evt-1", raw)

	var decoded episodicOverflowRef
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("expected valid JSON, got unmarshal error: %v\nraw output: %s", err, out)
	}
	if decoded.LogRef != "evt-1" {
		t.Errorf("expected log_ref evt-1, got %q", decoded.LogRef)
	}
	if decoded.Bytes != len(raw) {
		t.Errorf("expected bytes=%d, got %d", len(raw), decoded.Bytes)
	}
}

// TestTruncateEpisodicPayload_UTF8SafeTruncation 验证 512 字节截断不会切断多字节
// UTF-8 字符，产出的 preview 必须是合法 UTF-8。
func TestTruncateEpisodicPayload_UTF8SafeTruncation(t *testing.T) {
	em := NewEpisodicMem(nil)

	// 构造一个刚好在 512 字节边界附近全是 3 字节中文字符的 payload，
	// 强迫截断点大概率落在字符中间。
	raw := []byte(strings.Repeat("测", 300)) // 每个"测"3字节，共900字节 > 512
	out := em.truncateEpisodicPayload("evt-2", raw)

	var decoded episodicOverflowRef
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("expected valid JSON, got unmarshal error: %v\nraw output: %s", err, out)
	}
	if !utf8.ValidString(decoded.Preview) {
		t.Errorf("expected preview to be valid UTF-8, got %q", decoded.Preview)
	}
	if len(decoded.Preview) > 512 {
		t.Errorf("expected preview <= 512 bytes, got %d", len(decoded.Preview))
	}
}
