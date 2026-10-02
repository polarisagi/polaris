package retrieval

import (
	"testing"
	"time"
)

func TestEmbedBackoff_Sequence(t *testing.T) {
	b := NewEmbedBackoff()
	currentTime := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	b.nowFn = func() time.Time { return currentTime }

	// 初始状态
	if !b.CanAttempt() {
		t.Fatal("expected CanAttempt=true initially")
	}

	// 1st failure: delay = 0
	delay, stopped, failures := b.RecordFailure()
	if delay != 0 || stopped || failures != 1 {
		t.Fatalf("fail 1: got delay=%v, stopped=%v, failures=%d; want 0, false, 1", delay, stopped, failures)
	}
	if !b.CanAttempt() {
		t.Fatal("fail 1: expected CanAttempt=true")
	}

	// 2nd failure: delay = 0
	delay, stopped, failures = b.RecordFailure()
	if delay != 0 || stopped || failures != 2 {
		t.Fatalf("fail 2: got delay=%v, stopped=%v, failures=%d; want 0, false, 2", delay, stopped, failures)
	}
	if !b.CanAttempt() {
		t.Fatal("fail 2: expected CanAttempt=true")
	}

	// 3rd failure: delay = 5m
	delay, stopped, failures = b.RecordFailure()
	if delay != 5*time.Minute || stopped || failures != 3 {
		t.Fatalf("fail 3: got delay=%v, stopped=%v, failures=%d; want 5m, false, 3", delay, stopped, failures)
	}
	if b.CanAttempt() {
		t.Fatal("fail 3: expected CanAttempt=false immediately after 3rd failure")
	}

	// Advance clock by 4m -> still cannot attempt
	currentTime = currentTime.Add(4 * time.Minute)
	if b.CanAttempt() {
		t.Fatal("fail 3: expected CanAttempt=false at +4m")
	}

	// Advance clock by 1m (total +5m) -> can attempt
	currentTime = currentTime.Add(1 * time.Minute)
	if !b.CanAttempt() {
		t.Fatal("fail 3: expected CanAttempt=true at +5m")
	}

	// 4th failure: delay = 30m
	delay, stopped, failures = b.RecordFailure()
	if delay != 30*time.Minute || stopped || failures != 4 {
		t.Fatalf("fail 4: got delay=%v, stopped=%v, failures=%d; want 30m, false, 4", delay, stopped, failures)
	}
	if b.CanAttempt() {
		t.Fatal("fail 4: expected CanAttempt=false")
	}

	currentTime = currentTime.Add(30 * time.Minute)
	if !b.CanAttempt() {
		t.Fatal("fail 4: expected CanAttempt=true after 30m")
	}

	// 5th failure: delay = 2h
	delay, stopped, failures = b.RecordFailure()
	if delay != 2*time.Hour || stopped || failures != 5 {
		t.Fatalf("fail 5: got delay=%v, stopped=%v, failures=%d; want 2h, false, 5", delay, stopped, failures)
	}
	if b.CanAttempt() {
		t.Fatal("fail 5: expected CanAttempt=false")
	}

	currentTime = currentTime.Add(2 * time.Hour)
	if !b.CanAttempt() {
		t.Fatal("fail 5: expected CanAttempt=true after 2h")
	}

	// 6th failure: stopped
	delay, stopped, failures = b.RecordFailure()
	if delay != 0 || !stopped || failures != 6 {
		t.Fatalf("fail 6: got delay=%v, stopped=%v, failures=%d; want 0, true, 6", delay, stopped, failures)
	}
	if b.CanAttempt() {
		t.Fatal("fail 6: expected CanAttempt=false when stopped")
	}
	if !b.IsStopped() {
		t.Fatal("fail 6: expected IsStopped=true")
	}

	// Even advancing time 10 days, still stopped
	currentTime = currentTime.Add(240 * time.Hour)
	if b.CanAttempt() {
		t.Fatal("expected CanAttempt=false while stopped even after 10 days")
	}

	// 成功清零
	b.RecordSuccess()
	if b.ConsecutiveFailures() != 0 || b.IsStopped() || !b.CanAttempt() {
		t.Fatalf("expected RecordSuccess to reset everything, got failures=%d, stopped=%v, canAttempt=%v",
			b.ConsecutiveFailures(), b.IsStopped(), b.CanAttempt())
	}

	// 触发失败后 Reset() 恢复
	for i := 0; i < 6; i++ {
		b.RecordFailure()
	}
	if !b.IsStopped() || b.CanAttempt() {
		t.Fatal("expected stopped after 6 failures")
	}

	b.Reset()
	if b.ConsecutiveFailures() != 0 || b.IsStopped() || !b.CanAttempt() {
		t.Fatalf("expected Reset to restore everything, got failures=%d, stopped=%v, canAttempt=%v",
			b.ConsecutiveFailures(), b.IsStopped(), b.CanAttempt())
	}
}
