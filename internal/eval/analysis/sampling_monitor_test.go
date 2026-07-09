package analysis

import (
	"testing"
)

func TestContinuousSamplingMonitor_NoDegradation(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	// 填满窗口，全高分
	for i := 0; i < windowSize; i++ {
		m.RecordSample(0.95)
	}
	degraded, alert := m.CheckDegradation()
	if degraded || alert != nil {
		t.Fatalf("no degradation expected: degraded=%v alert=%v", degraded, alert)
	}
}

func TestContinuousSamplingMonitor_Degradation(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)
	// 建立基线
	for i := 0; i < windowSize; i++ {
		m.RecordSample(0.9)
	}
	m.CheckDegradation() // 触发基线快照

	// 退化到 0.7（< 0.9 × 0.9 = 0.81）
	for i := 0; i < windowSize; i++ {
		m.window[i] = 0.7
	}
	m.windowFilled = true

	degraded, alert := m.CheckDegradation()
	if !degraded {
		t.Fatal("expected degradation")
	}
	if alert == nil {
		t.Fatal("expected non-nil alert")
	}
	if alert.Attribution != CausalInternal {
		t.Errorf("expected CausalInternal, got %v", alert.Attribution)
	}
}

func TestContinuousSamplingMonitor_Attribution(t *testing.T) {
	m := NewContinuousSamplingMonitor(nil)

	// 注入 sevenDayAvg=0.9
	m.mu.Lock()
	m.sevenDayAvg = 0.9
	m.mu.Unlock()

	// 当前 current=0.7，断言归因为 CausalInternal（drop=0.22>0.15）
	attr1 := m.attributeLocked(0.7)
	if attr1 != CausalInternal {
		t.Errorf("expected CausalInternal, got %v", attr1)
	}

	// 注入 sevenDayAvg=0.9，current=0.86，断言归因为 CausalExternal（drop≈0.04<0.15）
	attr2 := m.attributeLocked(0.86)
	if attr2 != CausalExternal {
		t.Errorf("expected CausalExternal, got %v", attr2)
	}
}
