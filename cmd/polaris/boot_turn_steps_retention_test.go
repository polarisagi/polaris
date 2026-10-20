package main

import (
	"context"
	"testing"
	"time"
)

type fakeStepsPruner struct {
	called bool
	before int64
}

func (f *fakeStepsPruner) PurgeTurnStepsBefore(_ context.Context, before int64) (int64, error) {
	f.called, f.before = true, before
	return 3, nil
}

func TestPruneTurnStepsByRetention(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	f := &fakeStepsPruner{}
	if n, err := pruneTurnStepsByRetention(context.Background(), f, 0, now); err != nil || n != 0 || f.called {
		t.Fatalf("0 天 = 永久保留，不应触碰存储: n=%d err=%v called=%v", n, err, f.called)
	}
	if n, err := pruneTurnStepsByRetention(context.Background(), f, 30, now); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if want := now.AddDate(0, 0, -30).UnixMilli(); f.before != want {
		t.Errorf("cutoff = %d, want %d", f.before, want)
	}
	if n, err := pruneTurnStepsByRetention(context.Background(), nil, 30, now); err != nil || n != 0 {
		t.Fatalf("nil pruner 应安全跳过: n=%d err=%v", n, err)
	}
}
