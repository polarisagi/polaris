package fsm

import "testing"

func TestFormatUsageLine(t *testing.T) {
	got := formatUsageLine(12345, 128000, 20000, 17000, 0.0123, 0.0456, 12.7)
	want := "[usage] ctx=12.3k/128.0k cache=85% turn=$0.0123 session=$0.0456 wall=12s"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := formatUsageLine(500, 0, 0, 0, 0, 0, 0); got != "[usage] ctx=500/0 cache=- turn=$0.0000 session=$0.0000 wall=0s" {
		t.Fatalf("零值: %q", got)
	}
}

func TestNotePrefixBreak_FirstReasonWinsAndTakeClears(t *testing.T) {
	s := &StateContext{}
	s.NotePrefixBreakLocked("model_switch")
	s.NotePrefixBreakLocked("observation_mask")
	if got := s.TakePrefixBreakLocked(); got != "model_switch" {
		t.Fatalf("got %q", got)
	}
	if got := s.TakePrefixBreakLocked(); got != "" {
		t.Fatalf("取出后应清空: %q", got)
	}
}
