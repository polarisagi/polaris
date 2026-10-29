package fsm

import (
	"testing"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 插话并入全局污点按各自等级取最大值：owner 插话(Low)不抬升，外部插话(High)抬到 High，且只升不降。
func TestDrainSteerQueue_RaisesGlobalTaintByMaxLevel(t *testing.T) {
	mk := func(level types.TaintLevel) taint.TaintedString {
		return taint.NewTaintedString("x", taint.TaintSource{Module: "session", OriginTaintLevel: level}, "steer")
	}
	s := &StateContext{}
	s.LoopSteerQueue = []taint.TaintedString{mk(types.TaintLow)}
	if !s.DrainSteerQueueToMessages() {
		t.Fatal("expected steer to be consumed")
	}
	if s.GlobalTaintLevel >= types.TaintMedium {
		t.Fatalf("owner steer must not raise global taint to Medium+, got %v", s.GlobalTaintLevel)
	}
	s.LoopSteerQueue = []taint.TaintedString{mk(types.TaintLow), mk(types.TaintHigh)}
	s.DrainSteerQueueToMessages()
	if s.GlobalTaintLevel != types.TaintHigh {
		t.Fatalf("external steer must raise global taint to High, got %v", s.GlobalTaintLevel)
	}
	s.LoopSteerQueue = []taint.TaintedString{mk(types.TaintLow)}
	s.DrainSteerQueueToMessages()
	if s.GlobalTaintLevel != types.TaintHigh {
		t.Fatalf("global taint must never decrease, got %v", s.GlobalTaintLevel)
	}
}
