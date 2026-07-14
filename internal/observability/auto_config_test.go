package observability

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestNewAutoConfig_TierAssignment(t *testing.T) {
	tests := []struct {
		name     string
		totalRAM uint64
		wantTier probe.Tier
	}{
		{"Tier0_8GB", 8 * 1024 * 1024 * 1024, probe.Tier0},
		{"Tier1_16GB", 16 * 1024 * 1024 * 1024, probe.Tier1},
		{"Tier2_24GB", 24 * 1024 * 1024 * 1024, probe.Tier2},
		{"Tier3_64GB", 64 * 1024 * 1024 * 1024, probe.Tier3},
		{"Tier0_6GB_degraded", 6 * 1024 * 1024 * 1024, probe.Tier0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use totalRAM as availableRAM for testing (50% free)
			avail := tt.totalRAM / 2
			hp := probe.NewHardwareProbe(tt.totalRAM, avail)
			if hp.Tier != tt.wantTier {
				t.Errorf("totalRAM=%dGB → tier=%d, want tier=%d",
					tt.totalRAM/(1024*1024*1024), hp.Tier, tt.wantTier)
			}
		})
	}
}

func TestFeatureGate_TierGating(t *testing.T) {
	// Simulate HT0: 8GB total, 3GB available
	hp := probe.NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(8 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	// HT0 should NOT have QLoRA, PRM, large local models
	if fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT0 should not have QLoRA enabled")
	}
	if fg.IsEnabled(probe.FeaturePRMTraining) {
		t.Error("HT0 should not have PRM training enabled")
	}
	if fg.IsEnabled(probe.FeatureLargeLocalLLM) {
		t.Error("HT0 should not have large local LLM enabled")
	}
	// HT0 should have L2 sandbox and local embedding (degraded or enabled)
	if !fg.IsEnabled(probe.FeatureL2Sandbox) {
		t.Error("HT0 should have L2 sandbox enabled")
	}
}

func TestFeatureGate_Tier1MemoryPressure(t *testing.T) {
	// Simulate HT1: 16GB total, 8GB available
	hp := probe.NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(16 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	// HT1 with sufficient memory should have QLoRA
	if !fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT1 with 8GB free should have QLoRA enabled")
	}

	// Simulate memory pressure: drop to 3GB available
	fg.Reassess(3 * 1024)
	if fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT1 with 3GB free should have QLoRA disabled")
	}
}

func TestFeatureGate_DegradationOrder(t *testing.T) {
	hp := probe.NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(32 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	order := fg.DegradationOrder()
	if len(order) < 5 {
		t.Fatal("degradation order too short")
	}
	// PRM training (priority 60) should degrade first; L2 sandbox (priority 5) last
	if order[0] != probe.FeaturePRMTraining {
		t.Errorf("first to degrade should be PRMTraining, got %s", order[0])
	}
	lastIdx := len(order) - 1
	if order[lastIdx] != probe.FeatureL2Sandbox {
		t.Errorf("last to degrade should be L2Sandbox, got %s", order[lastIdx])
	}
}

func TestAutoConfig_StorageEngineSelection(t *testing.T) {
	// 三轴架构: 全 probe.Tier 均启用 sqlite + surreal
	for _, tc := range []struct {
		name     string
		totalRAM uint64
	}{
		{"HT0_8GB", 8 * 1024 * 1024 * 1024},
		{"HT1_16GB", 16 * 1024 * 1024 * 1024},
		{"HT2_32GB", 32 * 1024 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hp := probe.NewHardwareProbe(tc.totalRAM, tc.totalRAM/2)
			guard := probe.NewOSMemoryGuard(tc.totalRAM / (1024 * 1024))
			fg := probe.NewFeatureGate(hp, guard)
			probe.SetGlobalFeatureGate(fg)

			ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}
			ac.computeConfig()

			hasSQLite, hasSurreal := false, false
			for _, engine := range ac.Config.StorageEngines {
				switch engine {
				case "sqlite":
					hasSQLite = true
				case "surreal":
					hasSurreal = true
				}
			}
			if !hasSQLite {
				t.Errorf("%s: missing sqlite engine", tc.name)
			}
			if !hasSurreal {
				t.Errorf("%s: missing surreal engine", tc.name)
			}

		})
	}
}

func TestTierHelpers(t *testing.T) {
	// QLoRA model tier selection
	model, ok := probe.TierQLoRAModel(probe.Tier0)
	if ok || model != "" {
		t.Error("probe.Tier0 should not support QLoRA")
	}
	model, ok = probe.TierQLoRAModel(probe.Tier1)
	if !ok || model != "1-3B" {
		t.Errorf("probe.Tier1 QLoRA: want 1-3B/true, got %s/%v", model, ok)
	}
	model, ok = probe.TierQLoRAModel(probe.Tier2)
	if !ok || model != "7B" {
		t.Errorf("probe.Tier2 QLoRA: want 7B/true, got %s/%v", model, ok)
	}

	// Local model tier selection
	modelID, ok := probe.TierLocalModel(probe.Tier0)
	if ok || modelID != "Qwen3-3B-Q4_K_M" {
		t.Errorf("probe.Tier0 local model: want Qwen3-3B-Q4_K_M/false, got %s/%v", modelID, ok)
	}
	modelID, ok = probe.TierLocalModel(probe.Tier1)
	if !ok || modelID != "Qwen3-8B-Q4_K_M" {
		t.Errorf("probe.Tier1 local model: want Qwen3-8B-Q4_K_M/true, got %s/%v", modelID, ok)
	}
}

func TestSandboxPlatformConfig(t *testing.T) {
	available, backend := probe.TierSandboxConfig(probe.Tier1, "darwin")
	if !available || backend != "virtualization_framework" {
		t.Errorf("probe.Tier1 darwin: want true/virtualization_framework, got %v/%s", available, backend)
	}

	available, backend = probe.TierSandboxConfig(probe.Tier2, "linux")
	if !available || backend != "firecracker" {
		t.Errorf("probe.Tier2 linux: want true/firecracker, got %v/%s", available, backend)
	}

	available, _ = probe.TierSandboxConfig(probe.Tier0, "linux")
	if available {
		t.Error("probe.Tier0 should not have L3 sandbox")
	}
}

func TestFeatureGate_Override(t *testing.T) {
	hp := probe.NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(8 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	if fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT0: QLoRA should be disabled by default")
	}

	// Admin force-enables QLoRA
	fg.Override(probe.FeatureQLoRA, probe.FeatureEnabled)
	if !fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT0: QLoRA should be enabled after override")
	}

	// Clear override
	fg.Override(probe.FeatureQLoRA, probe.FeatureState(-1))
	if fg.IsEnabled(probe.FeatureQLoRA) {
		t.Error("HT0: QLoRA should be disabled after clearing override")
	}
}

func TestOSMemoryGuard_DegradationLevels(t *testing.T) {
	guard := probe.NewOSMemoryGuard(8 * 1024) // 8GB total

	tests := []struct {
		availableMB uint64
		wantLevel   probe.DegradationLevel
	}{
		{2048, probe.DegradationNone},
		{1400, probe.DegradationCaution},
		{900, probe.DegradationWarning},
		{400, probe.DegradationCritical},
	}
	for _, tt := range tests {
		got := guard.CheckAndProtect(tt.availableMB)
		if got != tt.wantLevel {
			t.Errorf("availableMB=%d → level=%d, want %d", tt.availableMB, got, tt.wantLevel)
		}
	}
}

func TestMemoryBudget_Scaling(t *testing.T) {
	// HT0: verify budget doesn't exceed available
	hp := probe.NewHardwareProbe(8*1024*1024*1024, 2*1024*1024*1024) // only 2GB available
	guard := probe.NewOSMemoryGuard(8 * 1024)
	fg := probe.NewFeatureGate(hp, guard)
	probe.SetGlobalFeatureGate(fg)

	ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}
	ac.computeConfig()

	b := ac.Config.MemoryBudgetDetails
	totalAllocated := b.ReservedMB + b.AgentRuntimeMB + b.LocalModelsMB + b.StorageMB + b.SandboxMB
	// After scaling, total should not exceed available
	if totalAllocated > ac.Config.AvailableRAMMB+512 { // +512MB buffer for rounding
		t.Errorf("memory budget %dMB exceeds available %dMB after scaling",
			totalAllocated, ac.Config.AvailableRAMMB)
	}
	// HT0 should have 0 local model budget
	if b.LocalModelsMB != 0 {
		t.Errorf("HT0 local model budget should be 0, got %dMB", b.LocalModelsMB)
	}
}

// Test new Bucket B features added in auto-config expansion.
func TestFeatureGate_BucketBNewFeatures(t *testing.T) {
	// HT0（8GB，3GB 可用）: LogicCollapse/GraphRAGFull/DeepRAG 已下放至 probe.Tier0，应 enabled。
	// ActivationSteer（需本地模型）、PresidioPII（probe.Tier1 PII sidecar）仍 disabled。
	hp := probe.NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(8 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	if !fg.IsEnabled(probe.FeatureLogicCollapse) {
		t.Error("HT0: LogicCollapse should be enabled (MinTier=probe.Tier0, avail 3GB > MinMemoryMB 1GB)")
	}
	if !fg.IsEnabled(probe.FeatureGraphRAGFull) {
		t.Error("HT0: GraphRAGFull should be enabled (MinTier=probe.Tier0, avail 3GB > MinMemoryMB 1GB)")
	}
	if !fg.IsEnabled(probe.FeatureDeepRAG) {
		t.Error("HT0: DeepRAG should be enabled (MinTier=probe.Tier0, avail 3GB > MinMemoryMB 1GB)")
	}
	if fg.IsEnabled(probe.FeatureActivationSteer) {
		t.Error("HT0: ActivationSteer should be disabled (no local model)")
	}
	if fg.IsEnabled(probe.FeaturePresidioPII) {
		t.Error("HT0: PresidioPII should be disabled (MinTier=probe.Tier1)")
	}

	// HT2（32GB）: 以上全部 enabled
	hp2 := probe.NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard2 := probe.NewOSMemoryGuard(32 * 1024)
	fg2 := probe.NewFeatureGate(hp2, guard2)

	if !fg2.IsEnabled(probe.FeatureLogicCollapse) {
		t.Error("HT2: LogicCollapse should be enabled")
	}
	if !fg2.IsEnabled(probe.FeatureGraphRAGFull) {
		t.Error("HT2: GraphRAGFull should be enabled")
	}
	if !fg2.IsEnabled(probe.FeatureDeepRAG) {
		t.Error("HT2: DeepRAG should be enabled")
	}
}

func TestFeatureGate_ComputerUseGUI_DisplayCheck(t *testing.T) {
	hp := probe.NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(16 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	// On macOS, should always be available (has display)
	if runtime.GOOS == "darwin" {
		if !fg.IsEnabled(probe.FeatureComputerUseGUI) {
			t.Error("macOS: ComputerUseGUI should be enabled")
		}
	}
	// On Linux without DISPLAY, should be disabled
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		if fg.IsEnabled(probe.FeatureComputerUseGUI) {
			t.Error("headless Linux: ComputerUseGUI should be disabled")
		}
	}
}

func TestFeatureGate_CrossFeatureDependencies(t *testing.T) {
	// ActivationSteer requires local inference
	hp := probe.NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(32 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	if !fg.IsEnabled(probe.FeatureActivationSteer) {
		t.Error("HT2: ActivationSteer should be enabled when local inference is available")
	}

	// Force-disable local inference → ActivationSteer should cascade-disable
	fg.Override(probe.FeatureLocalInference, probe.FeatureDisabled)
	// Reassess to recompute cross-feature dependencies
	fg.Reassess(16 * 1024)
	if fg.IsEnabled(probe.FeatureActivationSteer) {
		t.Error("ActivationSteer should be disabled when local inference is disabled")
	}
	fg.Override(probe.FeatureLocalInference, probe.FeatureState(-1))
}

func TestTierParameters_AllTiers(t *testing.T) {
	gb := uint64(1024 * 1024 * 1024)
	tests := []struct {
		totalRAM          uint64
		wantMaxDAG        int
		wantAgents        int
		wantScriptWorkers int
	}{
		{8 * gb, 4, 3, 4},
		{16 * gb, 8, 5, 8},
		{24 * gb, 12, 8, 12},
		{64 * gb, 16, 12, 16},
	}
	for _, tt := range tests {
		hp := probe.NewHardwareProbe(tt.totalRAM, tt.totalRAM/2)
		var p probe.TierParameters
		ac := &AutoConfig{Probe: hp}
		ac.computeTierParameters(&p)

		tierName := fmt.Sprintf("RAM=%dGB", tt.totalRAM/gb)
		if p.MaxConcurrentDAGNodes != tt.wantMaxDAG {
			t.Errorf("%s: MaxConcurrentDAGNodes=%d, want %d", tierName, p.MaxConcurrentDAGNodes, tt.wantMaxDAG)
		}
		if p.MaxAgents != tt.wantAgents {
			t.Errorf("%s: MaxAgents=%d, want %d", tierName, p.MaxAgents, tt.wantAgents)
		}
		if p.ScriptWorkerMax != tt.wantScriptWorkers {
			t.Errorf("%s: ScriptWorkerMax=%d, want %d", tierName, p.ScriptWorkerMax, tt.wantScriptWorkers)
		}
	}
}

// 2026-07-14（ADR-0051）：TierParameters.Param 已删除（全仓零生产调用点，字段
// 直接读取即可），TestTierParameters_ParamLookup 随之删除。

func TestAutoConfig_FeatureMap_AllFeatures(t *testing.T) {
	hp := probe.NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(32 * 1024)
	fg := probe.NewFeatureGate(hp, guard)
	probe.SetGlobalFeatureGate(fg)

	ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}
	ac.computeConfig()

	// 22 features: 原 17 + Embedding 阶梯(HQ/Ultra/Max) + STT/TTS 分级(HQSTT/LocalTTS)
	expectedFeatures := 22
	if len(ac.Config.Features) != expectedFeatures {
		t.Errorf("FeatureMap size: got %d, want %d", len(ac.Config.Features), expectedFeatures)
	}

	// Params should be populated
	if ac.Config.Params.MaxConcurrentDAGNodes == 0 {
		t.Error("Params not populated: MaxConcurrentDAGNodes is 0")
	}
	if ac.Config.Params.PoolIntentHandler == 0 {
		t.Error("Params not populated: PoolIntentHandler is 0")
	}
}

func TestFeatureGate_DegradationOrder_Complete(t *testing.T) {
	hp := probe.NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(32 * 1024)
	fg := probe.NewFeatureGate(hp, guard)

	order := fg.DegradationOrder()
	expectedLen := 0
	if len(order) == expectedLen {
		t.Errorf("DegradationOrder length: got %d, want %d", len(order), expectedLen)
	}
	// PRM should be first to degrade
	if order[0] != probe.FeaturePRMTraining {
		t.Errorf("first: got %s, want %s", order[0], probe.FeaturePRMTraining)
	}
	// L2Sandbox should be last to degrade
	if order[len(order)-1] != probe.FeatureL2Sandbox {
		t.Errorf("last: got %s, want %s", order[len(order)-1], probe.FeatureL2Sandbox)
	}
}

func init() {
	_ = runtime.GOARCH
}

type mockSandboxController struct {
	disabled   bool
	killedIdle bool
	killedAll  bool
}

func (m *mockSandboxController) DisableNewInstances(disable bool) {
	m.disabled = disable
}

func (m *mockSandboxController) KillIdleSandboxes(ctx context.Context) {
	m.killedIdle = true
}

func (m *mockSandboxController) KillAllNonCritical(ctx context.Context) {
	m.killedAll = true
}

func TestAutoConfig_SandboxController(t *testing.T) {
	hp := probe.NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	guard := probe.NewOSMemoryGuard(16 * 1024)
	fg := probe.NewFeatureGate(hp, guard)
	ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}

	mockSC := &mockSandboxController{}
	ac.WithSandboxController(mockSC)

	// Test Warning (L2)
	ac.MemoryPressureCallback(2*1024, probe.DegradationWarning)
	if !mockSC.killedIdle {
		t.Error("expected KillIdleSandboxes to be called on probe.DegradationWarning")
	}

	// Test Critical (L3)
	// Force a diff > 256MB to bypass hysteresis
	ac.MemoryPressureCallback(16*1024, probe.DegradationNone)
	ac.MemoryPressureCallback(1*1024, probe.DegradationCritical)
	if !mockSC.killedAll {
		t.Error("expected KillAllNonCritical to be called on probe.DegradationCritical")
	}

	// Test Caution (L1)
	ac.MemoryPressureCallback(16*1024, probe.DegradationNone)
	ac.MemoryPressureCallback(5*1024, probe.DegradationCaution)
	if !mockSC.disabled {
		t.Error("expected DisableNewInstances(true) to be called on probe.DegradationCaution")
	}

	// Test None (Recovery)
	ac.MemoryPressureCallback(16*1024, probe.DegradationNone)
	if mockSC.disabled {
		t.Error("expected DisableNewInstances(false) to be called on probe.DegradationNone")
	}
}
