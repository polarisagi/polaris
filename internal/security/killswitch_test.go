package security

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestKillSwitch_InitialState_IsRunning(t *testing.T) {
	ks := NewKillSwitch("", nil)
	if state := ks.GetState(); state != KillNormal {
		t.Errorf("Expected Initial State to be %v, got %v", KillNormal, state)
	}
	if ks.IsSealed() {
		t.Error("Expected IsSealed to be false")
	}
	if ks.IsFullStopped() {
		t.Error("Expected IsFullStopped to be false")
	}
}

func TestKillSwitch_CheckAndAct_NoPressure_StaysRunning(t *testing.T) {
	ks := NewKillSwitch("", nil)
	state := ks.CheckAndAct()
	if state != KillNormal {
		t.Errorf("Expected state to be KillNormal, got %v", state)
	}
}

func TestKillSwitch_ShouldThrottle_HighTokenRate(t *testing.T) {
	ks := NewKillSwitch("", nil)

	for i := 0; i < 6; i++ {
		ks.ReportError()
	}

	state := ks.CheckAndAct()
	if state != KillThrottle {
		t.Errorf("Expected KillThrottle, got %v", state)
	}
}

func TestKillSwitch_ShouldPause_VeryHighRate(t *testing.T) {
	ks := NewKillSwitch("", nil)

	// Trigger Pause by reporting safety violation
	ks.ReportSafetyViolation(false)

	state := ks.CheckAndAct()
	if state != KillPause {
		t.Errorf("Expected KillPause, got %v", state)
	}
}

func TestKillSwitch_ShouldFullStop_ExceedsLimit(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)

	// Trigger FullStop by reporting fatal violation
	ks.ReportSafetyViolation(true)

	state := ks.CheckAndAct()
	if state != KillFullStop {
		t.Errorf("Expected KillFullStop, got %v", state)
	}
}

func TestKillSwitch_TransitionLocked_MonotonicallyIncreases(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)

	// Trigger Throttle
	for i := 0; i < 6; i++ {
		ks.ReportError()
	}
	if state := ks.GetState(); state != KillThrottle {
		t.Errorf("Expected KillThrottle, got %v", state)
	}

	// Trigger Pause
	ks.ReportSafetyViolation(false)
	if state := ks.GetState(); state != KillPause {
		t.Errorf("Expected KillPause, got %v", state)
	}

	// Trigger FullStop
	ks.ReportSafetyViolation(true)
	if state := ks.GetState(); state != KillFullStop {
		t.Errorf("Expected KillFullStop, got %v", state)
	}
}

func TestKillSwitch_WriteFullStopFile_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)
	ks.ManualFullStop("admin", "test write file")

	path := filepath.Join(dir, ".fullstop")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Expected .fullstop file to be created, err: %v", err)
	}
}

func TestKillSwitch_IsFullStopFilePresent_TrueWhenExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".fullstop")
	os.WriteFile(path, []byte("test"), 0600)

	if !IsFullStopFilePresent(dir) {
		t.Error("Expected IsFullStopFilePresent to be true")
	}
}

func TestKillSwitch_IsSealed_FalseBeforeStop(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)
	if ks.IsSealed() {
		t.Error("Expected IsSealed to be false initially")
	}
}

func TestKillSwitch_IsSealed_TrueAfterStop(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)
	ks.ManualFullStop("admin", "test")
	if !ks.IsSealed() {
		t.Error("Expected IsSealed to be true after FullStop")
	}
}

func TestKillSwitch_OnSIGINT_TriggersFullStop(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)
	ks.OnSIGINT()
	ks.OnSIGINT()
	ks.OnSIGINT()
	if !ks.IsFullStopped() {
		t.Error("Expected FullStop after 3 SIGINTs")
	}
}

func TestKillSwitch_CheckKILLSWITCHFile_TriggersFullStop(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)
	os.WriteFile(filepath.Join(dir, "KILLSWITCH"), []byte("die"), 0600)
	ks.CheckKILLSWITCHFile()
	if !ks.IsFullStopped() {
		t.Error("Expected FullStop after KILLSWITCH file detected")
	}
}

func TestKillSwitch_ManualRecover_ResetsState(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)
	ks.ManualFullStop("admin", "test")

	recovered := false
	ks.OnRecovery(func(ctx context.Context) {
		recovered = true
	})

	ks.ManualRecover(context.Background(), "admin", "recover")
	if ks.IsFullStopped() {
		t.Error("Expected not FullStopped after ManualRecover")
	}
	if !recovered {
		t.Error("Expected OnRecovery callback to be called")
	}
}

func TestKillSwitch_ReportIrreversibleAttempt_TriggersPause(t *testing.T) {
	ks := NewKillSwitch("", nil)
	ks.ReportIrreversibleAttempt()
	if state := ks.GetState(); state != KillPause {
		t.Errorf("Expected KillPause, got %v", state)
	}
}
