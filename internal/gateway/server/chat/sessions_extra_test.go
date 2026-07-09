package chat

import (
	"strings"
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

func TestNewSessionID(t *testing.T) {
	sID := newSessionID()
	if !strings.HasPrefix(sID, "sess_") {
		t.Errorf("expected sess_ prefix, got %s", sID)
	}
	if len(sID) != 37 {
		t.Errorf("expected length 37, got %d", len(sID))
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
