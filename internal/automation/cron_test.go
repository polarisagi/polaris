package automation

import (
	"testing"
	"time"
)

// ─── ParseCron ─────────────────────────────────────────────────────────────────

func TestParseCron_ValidExpr(t *testing.T) {
	tests := []struct {
		expr string
	}{
		{"* * * * *"},
		{"0 * * * *"},
		{"*/5 * * * *"},
		{"0 9 * * 1-5"},
		{"30 14 1,15 * *"},
		{"0 0 1 1 *"},
		{"*/15 */2 * * 0,6"},
	}
	for _, tt := range tests {
		s, err := ParseCron(tt.expr)
		if err != nil {
			t.Errorf("ParseCron(%q): %v", tt.expr, err)
		}
		if s == nil {
			t.Errorf("ParseCron(%q): nil schedule", tt.expr)
		}
	}
}

func TestParseCron_InvalidExpr(t *testing.T) {
	tests := []string{
		"",
		"* * * *",
		"* * * * * *",
		"a b c d e",
		"60 * * * *", // minute out of range
		"* 24 * * *", // hour out of range
		"* * 32 * *", // day out of range
		"* * * 13 *", // month out of range
		"* * * * 7",  // weekday out of range
	}
	for _, expr := range tests {
		_, err := ParseCron(expr)
		if err == nil {
			t.Errorf("ParseCron(%q) should fail", expr)
		}
	}
}

// ─── CronSchedule.NextAfter ────────────────────────────────────────────────────

func TestNextAfter_EveryMinute(t *testing.T) {
	s, _ := ParseCron("* * * * *")
	now := time.Date(2026, 5, 19, 10, 30, 0, 0, time.UTC)
	next := s.NextAfter(now)
	want := time.Date(2026, 5, 19, 10, 31, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("every minute: want %v, got %v", want, next)
	}
}

func TestNextAfter_EveryFiveMinutes(t *testing.T) {
	s, _ := ParseCron("*/5 * * * *")
	now := time.Date(2026, 5, 19, 10, 33, 0, 0, time.UTC)
	next := s.NextAfter(now)
	want := time.Date(2026, 5, 19, 10, 35, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("*/5: want %v, got %v", want, next)
	}
}

func TestNextAfter_SpecificHour(t *testing.T) {
	s, _ := ParseCron("0 9 * * *")
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	next := s.NextAfter(now)
	want := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("0 9: want %v, got %v", want, next)
	}
}

func TestNextAfter_Weekdays(t *testing.T) {
	s, _ := ParseCron("0 9 * * 1-5") // weekdays
	// 2026-05-19 is Tuesday
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	next := s.NextAfter(now)
	want := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("weekday morning: want %v, got %v", want, next)
	}
}

func TestNextAfter_FridayToMonday(t *testing.T) {
	s, _ := ParseCron("0 9 * * 1-5")
	// 2026-05-22 is Friday
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	next := s.NextAfter(now)
	want := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC) // next Monday
	if !next.Equal(want) {
		t.Errorf("Friday→Monday: want %v, got %v", want, next)
	}
}
