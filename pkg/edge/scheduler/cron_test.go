package scheduler

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

// ─── CronEvalCache ─────────────────────────────────────────────────────────────

func TestCronEvalCache_DualKey(t *testing.T) {
	cache := NewCronEvalCache()
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	nextUTC, err := cache.Next("0 9 * * *", "UTC", now)
	if err != nil {
		t.Fatalf("UTC eval: %v", err)
	}

	nextAsia, err := cache.Next("0 9 * * *", "Asia/Shanghai", now)
	if err != nil {
		t.Fatalf("Asia eval: %v", err)
	}

	if nextUTC.Equal(nextAsia) {
		t.Error("UTC and Asia/Shanghai should produce different NextRun times")
	}
}

func TestCronEvalCache_EmptyTZDefaultsUTC(t *testing.T) {
	cache := NewCronEvalCache()
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	next, err := cache.Next("0 9 * * *", "", now)
	if err != nil {
		t.Fatalf("empty tz: %v", err)
	}
	want := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("empty tz → UTC: want %v, got %v", want, next)
	}
}

func TestCronEvalCache_CacheHit(t *testing.T) {
	cache := NewCronEvalCache()
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	// First call populates cache
	first, _ := cache.Next("*/5 * * * *", "UTC", now)
	// Second call before the cached value should hit cache
	second, _ := cache.Next("*/5 * * * *", "UTC", now)
	if !first.Equal(second) {
		t.Error("cache should return same value on second call")
	}
}

func TestCronEvalCache_BadTZ(t *testing.T) {
	cache := NewCronEvalCache()
	_, err := cache.Next("* * * * *", "Mars/Olympus", time.Now())
	if err == nil {
		t.Error("invalid timezone should return error")
	}
}

// ─── StaggerDelay ──────────────────────────────────────────────────────────────

func TestStaggerDelay_Zero(t *testing.T) {
	if d := StaggerDelay(0); d != 0 {
		t.Errorf("stagger 0: want 0, got %v", d)
	}
}

func TestStaggerDelay_Negative(t *testing.T) {
	if d := StaggerDelay(-1); d != 0 {
		t.Errorf("stagger -1: want 0, got %v", d)
	}
}

func TestStaggerDelay_Bounded(t *testing.T) {
	for range 50 {
		d := StaggerDelay(100)
		if d < 0 || d >= 100*time.Millisecond {
			t.Errorf("stagger 100ms out of bounds: %v", d)
		}
	}
}

// ─── CronRunner ────────────────────────────────────────────────────────────────

func TestCronRunner_ReportError(t *testing.T) {
	cr := NewCronRunner()
	cr.ReportError("task1")
	cr.ReportError("task1")
	cr.ReportError("task1")
	cr.ReportError("task1")
	threshold := cr.ReportError("task1") // 5th
	if !threshold {
		t.Error("5th consecutive error should hit threshold")
	}
}

func TestCronRunner_ReportSuccessResets(t *testing.T) {
	cr := NewCronRunner()
	cr.ReportError("task1")
	cr.ReportError("task1")
	cr.ReportSuccess("task1")
	cr.ReportError("task1")
	cr.ReportError("task1")
	cr.ReportError("task1")
	cr.ReportError("task1")
	threshold := cr.ReportError("task1") // 5th after reset
	if !threshold {
		t.Error("should hit threshold after reset+5")
	}
}

func TestCronRunner_ReportErrorDifferentTasks(t *testing.T) {
	cr := NewCronRunner()
	cr.ReportError("taskA")
	cr.ReportError("taskA")
	cr.ReportError("taskB")
	cr.ReportError("taskA")
	cr.ReportError("taskA")
	threshold := cr.ReportError("taskA")
	if !threshold {
		t.Error("taskA 5th error should hit threshold")
	}
}

// ─── Disabled ──────────────────────────────────────────────────────────────────

func TestDisabled_Nil(t *testing.T) {
	task := &ScheduledTask{ID: "t1"}
	if Disabled(task) {
		t.Error("nil DisabledAt should not be disabled")
	}
}

func TestDisabled_InFuture(t *testing.T) {
	future := time.Now().Add(time.Hour)
	task := &ScheduledTask{ID: "t1", DisabledAt: &future}
	if Disabled(task) {
		t.Error("future DisabledAt should not be disabled yet")
	}
}

func TestDisabled_InPast(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	task := &ScheduledTask{ID: "t1", DisabledAt: &past}
	if !Disabled(task) {
		t.Error("past DisabledAt should be disabled")
	}
}
