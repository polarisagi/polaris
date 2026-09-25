package util

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestElideMiddle_WithinLimitUnchanged(t *testing.T) {
	s := "short"
	if got := ElideMiddle(s, 100); got != s {
		t.Fatalf("got %q, want unchanged", got)
	}
}

func TestElideMiddle_KeepsHeadAndTail(t *testing.T) {
	s := "HEAD" + strings.Repeat("x", 10_000) + "TAIL"
	got := ElideMiddle(s, 400)
	if len(got) > 400 {
		t.Fatalf("len %d exceeds limit 400", len(got))
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("head or tail lost: %q", got)
	}
	if !strings.Contains(got, "bytes omitted") {
		t.Fatalf("missing omission marker: %q", got)
	}
}

// 多字节字符不得被从中间切断：模型端出现乱码会让截断后的内容无法解读。
func TestElideMiddle_UTF8Boundaries(t *testing.T) {
	s := strings.Repeat("中", 5_000)
	got := ElideMiddle(s, 301)
	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8")
	}
	if len(got) > 301 {
		t.Fatalf("len %d exceeds limit 301", len(got))
	}
}

func TestElideMiddle_LimitSmallerThanMarker(t *testing.T) {
	s := strings.Repeat("a", 1_000)
	got := ElideMiddle(s, 5)
	if len(got) > 5 || !utf8.ValidString(got) {
		t.Fatalf("got %q, want <=5 valid bytes", got)
	}
}
