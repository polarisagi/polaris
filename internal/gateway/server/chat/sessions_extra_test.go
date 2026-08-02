package chat

import (
	"testing"
	"time"
)

func TestParseTaskDuration(t *testing.T) {
	now := time.Now()
	before := now.Add(-30 * time.Minute)

	nowStr := now.Format(time.RFC3339)
	beforeStr := before.Format(time.RFC3339)

	res := parseTaskDuration(beforeStr, nowStr)
	if res <= 0 {
		t.Errorf("expected positive duration")
	}

	res2 := parseTaskDuration("invalid", "invalid")
	if res2 != 0 {
		t.Errorf("expected 0 for invalid duration")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 3) != "hel" {
		t.Errorf("truncate error")
	}
	if truncate("hello", 10) != "hello" {
		t.Errorf("truncate error")
	}
	if truncate("hello", 5) != "hello" {
		t.Errorf("truncate error")
	}
}
