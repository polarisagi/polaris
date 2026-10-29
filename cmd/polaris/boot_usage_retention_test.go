package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakePruner struct {
	calls  int
	before int64
	n      int64
	err    error
}

func (f *fakePruner) PruneLLMCalls(_ context.Context, beforeMs int64) (int64, error) {
	f.calls++
	f.before = beforeMs
	return f.n, f.err
}

func TestPruneLLMCallsByRetention_CutoffAndDisabled(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	p := &fakePruner{n: 42}
	n, err := pruneLLMCallsByRetention(context.Background(), p, 90, now)
	if err != nil || n != 42 || p.calls != 1 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, p.calls)
	}
	if want := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC).UnixMilli(); p.before != want {
		t.Fatalf("90 天前的阈值应为 %d，got %d", want, p.before)
	}
	// 0 = 永久保留、nil pruner：都不得触库。
	p2 := &fakePruner{}
	if n, err := pruneLLMCallsByRetention(context.Background(), p2, 0, now); n != 0 || err != nil || p2.calls != 0 {
		t.Fatal("retention_days=0 不应清理")
	}
	if n, err := pruneLLMCallsByRetention(context.Background(), nil, 90, now); n != 0 || err != nil {
		t.Fatal("nil pruner 应为 no-op")
	}
}

func TestPruneLLMCallsByRetention_PropagatesError(t *testing.T) {
	p := &fakePruner{err: errors.New("db locked")}
	if _, err := pruneLLMCallsByRetention(context.Background(), p, 30, time.Now()); err == nil {
		t.Fatal("清理失败必须上抛给调用方记录，而不是吞掉")
	}
}
