package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/offpeak"
)

func gateAt(t *testing.T, now time.Time, windows ...string) *offpeak.Gate {
	t.Helper()
	w, err := offpeak.Parse(windows)
	if err != nil {
		t.Fatal(err)
	}
	return offpeak.NewGateWithClock(w, func() time.Time { return now }, time.After)
}

func TestNewOffpeakGate_DefaultEmptyIsNil(t *testing.T) {
	if g := newOffpeakGate(config.DefaultThresholds().M1Router); g != nil {
		t.Fatal("默认空窗口必须不错峰（nil Gate）")
	}
	bad := config.DefaultThresholds().M1Router
	bad.OffpeakWindows = []string{"nonsense"}
	if g := newOffpeakGate(bad); g != nil {
		t.Fatal("非法窗口应降级为不错峰而不是让后台停摆")
	}
	ok := config.DefaultThresholds().M1Router
	ok.OffpeakWindows = []string{"16:00-24:00"}
	if newOffpeakGate(ok) == nil {
		t.Fatal("合法窗口应构造出 Gate")
	}
}

// 窗口外：handler 不得执行，返回携带窗口起点的 OffPeakDeferral；窗口内与 nil Gate：原样执行。
func TestDeferOffPeak_OutsideWindowDefersUntilStart(t *testing.T) {
	ran := 0
	h := func(context.Context, *store.OutboxRecord) error { ran++; return nil }

	outside := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	err := deferOffPeak(gateAt(t, outside, "16:00-24:00"), h)(context.Background(), &store.OutboxRecord{})
	var d *protocol.OffPeakDeferral
	if !errors.As(err, &d) || ran != 0 {
		t.Fatalf("窗口外应推迟且不执行: err=%v ran=%d", err, ran)
	}
	if want := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC); !d.Until.Equal(want) {
		t.Fatalf("Until=%v want %v", d.Until, want)
	}

	inside := time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)
	if err := deferOffPeak(gateAt(t, inside, "16:00-24:00"), h)(context.Background(), &store.OutboxRecord{}); err != nil || ran != 1 {
		t.Fatalf("窗口内应原样执行: err=%v ran=%d", err, ran)
	}
	if err := deferOffPeak(nil, h)(context.Background(), &store.OutboxRecord{}); err != nil || ran != 2 {
		t.Fatalf("nil Gate 应原样执行: err=%v ran=%d", err, ran)
	}
}
