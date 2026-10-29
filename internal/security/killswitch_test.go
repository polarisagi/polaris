package security

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestKillSwitch_InitialState_IsRunning(t *testing.T) {
	ks := NewKillSwitch("", nil)
	if state := ks.GetState(); state != types.KillNormal {
		t.Errorf("Expected Initial State to be %v, got %v", types.KillNormal, state)
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
	if state != types.KillNormal {
		t.Errorf("Expected state to be types.KillNormal, got %v", state)
	}
}

func TestKillSwitch_ShouldThrottle_HighTokenRate(t *testing.T) {
	ks := NewKillSwitch("", nil)

	for i := 0; i < 6; i++ {
		ks.ReportError()
	}

	state := ks.CheckAndAct()
	if state != types.KillThrottle {
		t.Errorf("Expected types.KillThrottle, got %v", state)
	}
}

func TestKillSwitch_ShouldPause_VeryHighRate(t *testing.T) {
	ks := NewKillSwitch("", nil)

	// Trigger Pause by reporting safety violation
	ks.ReportSafetyViolation(false)

	state := ks.CheckAndAct()
	if state != types.KillPause {
		t.Errorf("Expected types.KillPause, got %v", state)
	}
}

func TestKillSwitch_ShouldFullStop_ExceedsLimit(t *testing.T) {
	ks := NewKillSwitch(t.TempDir(), nil)

	// Trigger FullStop by reporting fatal violation
	ks.ReportSafetyViolation(true)

	state := ks.CheckAndAct()
	if state != types.KillFullStop {
		t.Errorf("Expected types.KillFullStop, got %v", state)
	}
}

func TestKillSwitch_TransitionLocked_MonotonicallyIncreases(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)

	// Trigger Throttle
	for i := 0; i < 6; i++ {
		ks.ReportError()
	}
	if state := ks.GetState(); state != types.KillThrottle {
		t.Errorf("Expected types.KillThrottle, got %v", state)
	}

	// Trigger Pause
	ks.ReportSafetyViolation(false)
	if state := ks.GetState(); state != types.KillPause {
		t.Errorf("Expected types.KillPause, got %v", state)
	}

	// Trigger FullStop
	ks.ReportSafetyViolation(true)
	if state := ks.GetState(); state != types.KillFullStop {
		t.Errorf("Expected types.KillFullStop, got %v", state)
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
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)
	ks.ManualFullStop("admin", "test")

	recovered := false
	ks.OnRecovery(func(ctx context.Context) {
		recovered = true
	})

	err := ks.ManualRecover(context.Background(), "admin", "recover")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if ks.IsFullStopped() {
		t.Error("Expected not FullStopped after ManualRecover")
	}
	if !recovered {
		t.Error("Expected OnRecovery callback to be called")
	}
	if IsFullStopFilePresent(dir) {
		t.Error("Expected .fullstop file to be deleted")
	}
}

func TestKillSwitch_ManualRecover_FailsToDelete(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(dir, nil)
	ks.ManualFullStop("admin", "test")

	// Make the directory read-only so file deletion fails
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)

	err := ks.ManualRecover(context.Background(), "admin", "recover")
	if err == nil {
		t.Error("Expected error when deleting .fullstop fails")
	}
	if !ks.IsFullStopped() {
		t.Error("Expected to remain FullStopped when deletion fails")
	}
}

func TestKillSwitch_ReportIrreversibleAttempt_TriggersPause(t *testing.T) {
	ks := NewKillSwitch("", nil)
	ks.ReportIrreversibleAttempt()
	if state := ks.GetState(); state != types.KillPause {
		t.Errorf("Expected types.KillPause, got %v", state)
	}
}

// fakeClock 为 KillSwitch 与 TokenBurnRate 提供同一条确定性时间轴。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// burnHarness 构造"每秒入账 tokensPerSec"的 tbr + KillSwitch，返回推进一秒并检查的函数。
func burnHarness(t *testing.T) (*KillSwitch, *metrics.TokenBurnRate, *fakeClock, func(tokens int64) types.KillState) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	tbr := metrics.NewTokenBurnRateAt(clk.Now())
	ks := NewKillSwitch(t.TempDir(), tbr)
	ks.now = clk.Now
	step := func(tokens int64) types.KillState {
		tbr.Add(tokens)
		clk.Advance(time.Second)
		tbr.TickAt(clk.Now())
		return ks.CheckAndAct()
	}
	return ks, tbr, clk, step
}

// 回归（2026-10-28）：燃烧率极高也只能触发 Stage1，不得直写 .fullstop。
// 此前 EMA_30s > 10x 基线直接 FULLSTOP，一次带缓存的正常长对话就把进程永久封存。
func TestKillSwitch_BurnRateNeverFullStops(t *testing.T) {
	ks, _, _, step := burnHarness(t)
	var last types.KillState
	for i := 0; i < 120; i++ {
		last = step(1_000_000) // 远超任何基线倍数
	}
	if last != types.KillThrottle {
		t.Fatalf("燃烧率应只触发 Throttle，got %v", last)
	}
	if ks.IsSealed() || IsFullStopFilePresent(ks.dataDir) {
		t.Fatal("燃烧率不得封存")
	}
}

// Stage1 recovery: auto —— 燃烧率回落后自动回到 Normal，且回落后不再累计"持续 10min"。
func TestKillSwitch_ThrottleAutoRecoversWhenBurnSubsides(t *testing.T) {
	ks, _, clk, step := burnHarness(t)
	for i := 0; i < 30; i++ {
		step(200_000)
	}
	if ks.GetState() != types.KillThrottle {
		t.Fatalf("预期进入 Throttle，got %v", ks.GetState())
	}
	s := types.KillThrottle
	for i := 0; i < 300 && s != types.KillNormal; i++ {
		s = step(0)
	}
	if s != types.KillNormal {
		t.Fatalf("燃烧率回落后应自动回到 Normal，got %v", s)
	}
	if !ks.Allowed() {
		t.Fatal("Normal 应放行新任务")
	}
	clk.Advance(11 * time.Minute)
	if got := ks.CheckAndAct(); got != types.KillNormal {
		t.Fatalf("已回落的 Throttle 不得因历史时长升级 Pause，got %v", got)
	}
}

// 规范：Stage1 持续 > 10min → Pause；Pause 未审批 15min → FullStop。
func TestKillSwitch_SustainedThrottleEscalatesByTime(t *testing.T) {
	ks, _, clk, step := burnHarness(t)
	for i := 0; i < 30; i++ {
		step(200_000)
	}
	// 持续高燃烧 10 分钟（每秒推进，保持 EMA 在线）
	for i := 0; i < 11*60 && ks.GetState() == types.KillThrottle; i++ {
		step(200_000)
	}
	if ks.GetState() != types.KillPause {
		t.Fatalf("Throttle 持续 >10min 应升 Pause，got %v", ks.GetState())
	}
	// Pause 不自动回落，即使燃烧率归零
	for i := 0; i < 120; i++ {
		step(0)
	}
	if ks.GetState() != types.KillPause {
		t.Fatalf("Pause 必须人工恢复，got %v", ks.GetState())
	}
	clk.Advance(16 * time.Minute)
	if got := ks.CheckAndAct(); got != types.KillFullStop {
		t.Fatalf("Pause 15min 未审批应升 FullStop，got %v", got)
	}
	if !IsFullStopFilePresent(ks.dataDir) {
		t.Fatal("FullStop 应写 .fullstop")
	}
}

// 连续错误 >5 进入 Throttle；安静期过后计数清零并自动回落。
func TestKillSwitch_ErrorThrottleDecays(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	ks := NewKillSwitch(t.TempDir(), nil)
	ks.now = clk.Now
	for i := 0; i < 6; i++ {
		ks.ReportError()
	}
	if ks.GetState() != types.KillThrottle {
		t.Fatalf("got %v", ks.GetState())
	}
	clk.Advance(errorQuietReset + time.Second)
	if got := ks.CheckAndAct(); got != types.KillNormal {
		t.Fatalf("安静期后应自动回落，got %v", got)
	}
}
