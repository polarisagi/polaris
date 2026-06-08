package observability

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestNewAutoConfig_TierAssignment(t *testing.T) {
	tests := []struct {
		name     string
		totalRAM uint64
		wantTier Tier
	}{
		{"Tier0_8GB", 8 * 1024 * 1024 * 1024, Tier0},
		{"Tier1_16GB", 16 * 1024 * 1024 * 1024, Tier1},
		{"Tier2_24GB", 24 * 1024 * 1024 * 1024, Tier2},
		{"Tier3_64GB", 64 * 1024 * 1024 * 1024, Tier3},
		{"Tier0_6GB_degraded", 6 * 1024 * 1024 * 1024, Tier0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use totalRAM as availableRAM for testing (50% free)
			avail := tt.totalRAM / 2
			hp := NewHardwareProbe(tt.totalRAM, avail)
			if hp.Tier != tt.wantTier {
				t.Errorf("totalRAM=%dGB → tier=%d, want tier=%d",
					tt.totalRAM/(1024*1024*1024), hp.Tier, tt.wantTier)
			}
		})
	}
}

func TestFeatureGate_TierGating(t *testing.T) {
	// Simulate HT0: 8GB total, 3GB available
	hp := NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := NewOSMemoryGuard(8 * 1024)
	fg := NewFeatureGate(hp, guard)

	// HT0 should NOT have QLoRA, PRM, large local models
	if fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT0 should not have QLoRA enabled")
	}
	if fg.IsEnabled(FeaturePRMTraining) {
		t.Error("HT0 should not have PRM training enabled")
	}
	if fg.IsEnabled(FeatureLargeLocalLLM) {
		t.Error("HT0 should not have large local LLM enabled")
	}
	// HT0 should have L2 sandbox and local embedding (degraded or enabled)
	if !fg.IsEnabled(FeatureL2Sandbox) {
		t.Error("HT0 should have L2 sandbox enabled")
	}
}

func TestFeatureGate_Tier1MemoryPressure(t *testing.T) {
	// Simulate HT1: 16GB total, 8GB available
	hp := NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	guard := NewOSMemoryGuard(16 * 1024)
	fg := NewFeatureGate(hp, guard)

	// HT1 with sufficient memory should have QLoRA
	if !fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT1 with 8GB free should have QLoRA enabled")
	}

	// Simulate memory pressure: drop to 3GB available
	fg.Reassess(3 * 1024)
	if fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT1 with 3GB free should have QLoRA disabled")
	}
}

func TestFeatureGate_DegradationOrder(t *testing.T) {
	hp := NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := NewOSMemoryGuard(32 * 1024)
	fg := NewFeatureGate(hp, guard)

	order := fg.DegradationOrder()
	if len(order) < 5 {
		t.Fatal("degradation order too short")
	}
	// PRM training (priority 60) should degrade first; L2 sandbox (priority 5) last
	if order[0] != FeaturePRMTraining {
		t.Errorf("first to degrade should be PRMTraining, got %s", order[0])
	}
	lastIdx := len(order) - 1
	if order[lastIdx] != FeatureL2Sandbox {
		t.Errorf("last to degrade should be L2Sandbox, got %s", order[lastIdx])
	}
}

func TestAutoConfig_StorageEngineSelection(t *testing.T) {
	// 三轴架构: 全 Tier 均启用 sqlite + surreal
	for _, tc := range []struct {
		name     string
		totalRAM uint64
	}{
		{"HT0_8GB", 8 * 1024 * 1024 * 1024},
		{"HT1_16GB", 16 * 1024 * 1024 * 1024},
		{"HT2_32GB", 32 * 1024 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hp := NewHardwareProbe(tc.totalRAM, tc.totalRAM/2)
			guard := NewOSMemoryGuard(tc.totalRAM / (1024 * 1024))
			fg := NewFeatureGate(hp, guard)
			SetGlobalFeatureGate(fg)

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

			if ac.Config.SurrealVecMode != SurrealVecBrute && ac.Config.SurrealVecMode != SurrealVecHNSW {
				t.Errorf("%s: SurrealVecMode=%d out of range", tc.name, ac.Config.SurrealVecMode)
			}
		})
	}
}

func TestAutoConfig_SurrealVecMode(t *testing.T) {
	tests := []struct {
		name     string
		totalRAM uint64
		wantHNSW bool
	}{
		{"Tier0_8GB_bruteforce", 8 * 1024 * 1024 * 1024, false},
		{"Tier1_16GB_hnsw", 16 * 1024 * 1024 * 1024, true},
		{"Tier2_32GB_hnsw", 32 * 1024 * 1024 * 1024, true},
		{"Tier3_64GB_hnsw", 64 * 1024 * 1024 * 1024, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hp := NewHardwareProbe(tt.totalRAM, tt.totalRAM/2)
			guard := NewOSMemoryGuard(tt.totalRAM / (1024 * 1024))
			fg := NewFeatureGate(hp, guard)
			SetGlobalFeatureGate(fg)

			ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}
			ac.computeConfig()

			want := SurrealVecBrute
			if tt.wantHNSW {
				want = SurrealVecHNSW
			}
			if ac.Config.SurrealVecMode != want {
				t.Errorf("%s: SurrealVecMode=%d, want %d", tt.name, ac.Config.SurrealVecMode, want)
			}
		})
	}
}

func TestTierHelpers(t *testing.T) {
	// QLoRA model tier selection
	model, ok := TierQLoRAModel(Tier0)
	if ok || model != "" {
		t.Error("Tier0 should not support QLoRA")
	}
	model, ok = TierQLoRAModel(Tier1)
	if !ok || model != "1-3B" {
		t.Errorf("Tier1 QLoRA: want 1-3B/true, got %s/%v", model, ok)
	}
	model, ok = TierQLoRAModel(Tier2)
	if !ok || model != "7B" {
		t.Errorf("Tier2 QLoRA: want 7B/true, got %s/%v", model, ok)
	}

	// Local model tier selection
	modelID, ok := TierLocalModel(Tier0)
	if ok || modelID != "Qwen3-3B-Q4_K_M" {
		t.Errorf("Tier0 local model: want Qwen3-3B-Q4_K_M/false, got %s/%v", modelID, ok)
	}
	modelID, ok = TierLocalModel(Tier1)
	if !ok || modelID != "Qwen3-8B-Q4_K_M" {
		t.Errorf("Tier1 local model: want Qwen3-8B-Q4_K_M/true, got %s/%v", modelID, ok)
	}
}

func TestSandboxPlatformConfig(t *testing.T) {
	available, backend := TierSandboxConfig(Tier1, "darwin")
	if !available || backend != "virtualization_framework" {
		t.Errorf("Tier1 darwin: want true/virtualization_framework, got %v/%s", available, backend)
	}

	available, backend = TierSandboxConfig(Tier2, "linux")
	if !available || backend != "firecracker" {
		t.Errorf("Tier2 linux: want true/firecracker, got %v/%s", available, backend)
	}

	available, _ = TierSandboxConfig(Tier0, "linux")
	if available {
		t.Error("Tier0 should not have L3 sandbox")
	}
}

func TestFeatureGate_Override(t *testing.T) {
	hp := NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := NewOSMemoryGuard(8 * 1024)
	fg := NewFeatureGate(hp, guard)

	if fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT0: QLoRA should be disabled by default")
	}

	// Admin force-enables QLoRA
	fg.Override(FeatureQLoRA, FeatureEnabled)
	if !fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT0: QLoRA should be enabled after override")
	}

	// Clear override
	fg.Override(FeatureQLoRA, FeatureState(-1))
	if fg.IsEnabled(FeatureQLoRA) {
		t.Error("HT0: QLoRA should be disabled after clearing override")
	}
}

func TestOSMemoryGuard_DegradationLevels(t *testing.T) {
	guard := NewOSMemoryGuard(8 * 1024) // 8GB total

	tests := []struct {
		availableMB uint64
		wantLevel   DegradationLevel
	}{
		{2048, DegradationNone},
		{1400, DegradationCaution},
		{900, DegradationWarning},
		{400, DegradationCritical},
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
	hp := NewHardwareProbe(8*1024*1024*1024, 2*1024*1024*1024) // only 2GB available
	guard := NewOSMemoryGuard(8 * 1024)
	fg := NewFeatureGate(hp, guard)
	SetGlobalFeatureGate(fg)

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
	// HT0: all Bucket B features should be disabled
	hp := NewHardwareProbe(8*1024*1024*1024, 3*1024*1024*1024)
	guard := NewOSMemoryGuard(8 * 1024)
	fg := NewFeatureGate(hp, guard)

	if fg.IsEnabled(FeatureLogicCollapse) {
		t.Error("HT0: LogicCollapse should be disabled")
	}
	if fg.IsEnabled(FeatureActivationSteer) {
		t.Error("HT0: ActivationSteer should be disabled (no local model)")
	}
	if fg.IsEnabled(FeaturePresidioPII) {
		t.Error("HT0: PresidioPII should be disabled")
	}

	// HT2: should enable most Bucket B features
	hp2 := NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard2 := NewOSMemoryGuard(32 * 1024)
	fg2 := NewFeatureGate(hp2, guard2)

	if !fg2.IsEnabled(FeatureLogicCollapse) {
		t.Error("HT2: LogicCollapse should be enabled")
	}
	if !fg2.IsEnabled(FeatureGraphRAGFull) {
		t.Error("HT2: GraphRAGFull should be enabled")
	}
}

func TestFeatureGate_ComputerUseGUI_DisplayCheck(t *testing.T) {
	hp := NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	guard := NewOSMemoryGuard(16 * 1024)
	fg := NewFeatureGate(hp, guard)

	// On macOS, should always be available (has display)
	if runtime.GOOS == "darwin" {
		if !fg.IsEnabled(FeatureComputerUseGUI) {
			t.Error("macOS: ComputerUseGUI should be enabled")
		}
	}
	// On Linux without DISPLAY, should be disabled
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		if fg.IsEnabled(FeatureComputerUseGUI) {
			t.Error("headless Linux: ComputerUseGUI should be disabled")
		}
	}
}

func TestFeatureGate_CrossFeatureDependencies(t *testing.T) {
	// ActivationSteer requires local inference
	hp := NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := NewOSMemoryGuard(32 * 1024)
	fg := NewFeatureGate(hp, guard)

	if !fg.IsEnabled(FeatureActivationSteer) {
		t.Error("HT2: ActivationSteer should be enabled when local inference is available")
	}

	// Force-disable local inference → ActivationSteer should cascade-disable
	fg.Override(FeatureLocalInference, FeatureDisabled)
	// Reassess to recompute cross-feature dependencies
	fg.Reassess(16 * 1024)
	if fg.IsEnabled(FeatureActivationSteer) {
		t.Error("ActivationSteer should be disabled when local inference is disabled")
	}
	fg.Override(FeatureLocalInference, FeatureState(-1))
}

func TestTierParameters_AllTiers(t *testing.T) {
	gb := uint64(1024 * 1024 * 1024)
	tests := []struct {
		totalRAM   uint64
		wantMaxDAG int
		wantAgents int
		wantWasm   int
	}{
		{8 * gb, 4, 3, 4},
		{16 * gb, 8, 5, 8},
		{24 * gb, 12, 8, 12},
		{64 * gb, 16, 12, 16},
	}
	for _, tt := range tests {
		hp := NewHardwareProbe(tt.totalRAM, tt.totalRAM/2)
		var p TierParameters
		ac := &AutoConfig{Probe: hp}
		ac.computeTierParameters(&p)

		tierName := fmt.Sprintf("RAM=%dGB", tt.totalRAM/gb)
		if p.MaxConcurrentDAGNodes != tt.wantMaxDAG {
			t.Errorf("%s: MaxConcurrentDAGNodes=%d, want %d", tierName, p.MaxConcurrentDAGNodes, tt.wantMaxDAG)
		}
		if p.MaxAgents != tt.wantAgents {
			t.Errorf("%s: MaxAgents=%d, want %d", tierName, p.MaxAgents, tt.wantAgents)
		}
		if p.WasmPoolMax != tt.wantWasm {
			t.Errorf("%s: WasmPoolMax=%d, want %d", tierName, p.WasmPoolMax, tt.wantWasm)
		}
	}
}

func TestTierParameters_ParamLookup(t *testing.T) {
	var p TierParameters
	hp := NewHardwareProbe(16*1024*1024*1024, 8*1024*1024*1024)
	ac := &AutoConfig{Probe: hp}
	ac.computeTierParameters(&p)

	if v := p.Param("max_concurrent_dag_nodes"); v != 8 {
		t.Errorf("max_concurrent_dag_nodes: got %d, want 8", v)
	}
	if v := p.Param("wasm_pool_max"); v != 8 {
		t.Errorf("wasm_pool_max: got %d, want 8", v)
	}
	if v := p.Param("nonexistent"); v != 0 {
		t.Errorf("nonexistent: got %d, want 0", v)
	}
}

func TestAutoConfig_FeatureMap_AllFeatures(t *testing.T) {
	hp := NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := NewOSMemoryGuard(32 * 1024)
	fg := NewFeatureGate(hp, guard)
	SetGlobalFeatureGate(fg)

	ac := &AutoConfig{Probe: hp, Guard: guard, Gate: fg}
	ac.computeConfig()

	// 15 features: 新增 FeatureLocalSTT（本地 STT，sherpa-onnx SenseVoice）
	expectedFeatures := 15
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
	hp := NewHardwareProbe(32*1024*1024*1024, 16*1024*1024*1024)
	guard := NewOSMemoryGuard(32 * 1024)
	fg := NewFeatureGate(hp, guard)

	order := fg.DegradationOrder()
	if len(order) != 15 {
		t.Errorf("DegradationOrder length: got %d, want 15", len(order))
	}
	// PRM should be first to degrade
	if order[0] != FeaturePRMTraining {
		t.Errorf("first: got %s, want %s", order[0], FeaturePRMTraining)
	}
	// L2Sandbox should be last to degrade
	if order[len(order)-1] != FeatureL2Sandbox {
		t.Errorf("last: got %s, want %s", order[len(order)-1], FeatureL2Sandbox)
	}
}

func init() {
	_ = runtime.GOARCH
}
