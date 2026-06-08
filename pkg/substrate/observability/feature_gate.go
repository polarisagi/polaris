package observability

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
)

// Feature represents a subsystem that can be auto-enabled/disabled based on hardware.
type Feature string

const (
	FeatureLocalInference Feature = "local_inference" // M1: local model loading
	FeatureLocalEmbedding Feature = "local_embedding" // M1: local embedding model
	FeatureLocalSTT       Feature = "local_stt"       // M13: sherpa-onnx 本地语音识别（SenseVoice）
	FeatureQLoRA          Feature = "qlora"           // M9: QLoRA gradient training
	FeaturePRMTraining    Feature = "prm_training"    // M9: PRM trainer worker
	FeatureL3Sandbox      Feature = "l3_sandbox"      // M7: microVM sandbox (Firecracker/VZ)
	FeatureL2Sandbox      Feature = "l2_sandbox"      // M7: Wasmtime sandbox
	FeatureGraphRAGFull   Feature = "graphrag_full"   // M10: Leiden + KuzuDB + LLM community summary
	FeatureSurrealDBCore  Feature = "surrealdb_core"  // M2: SurrealDB-Core 认知轴 (KV+HNSW+BM25+图，CGO-Free FFI)
	FeatureLargeLocalLLM  Feature = "large_local_llm" // M1: 7B+ local model
	// 桶 B — 新增 5 个特性（原为 Tier 硬编码，现自动检测）
	FeatureLogicCollapse   Feature = "logic_collapse"   // M6/M9: System 2→System 1 distillation, TinyGo compile
	FeatureComputerUseGUI  Feature = "computer_use_gui" // M7: GUI automation (VLM + screen control)
	FeaturePresidioPII     Feature = "presidio_pii"     // M11: Microsoft Presidio NER sidecar for PII detection
	FeatureWebUI           Feature = "web_ui"           // M13: go:embed HTMX Web dashboard
	FeatureActivationSteer Feature = "activation_steer" // M9: Activation Steering (hidden_state injection)
)

// FeatureState describes the current availability of a feature.
type FeatureState int32

const (
	FeatureEnabled  FeatureState = 0 // fully available
	FeatureDegraded FeatureState = 1 // available but with reduced capacity
	FeatureDisabled FeatureState = 2 // unavailable due to hardware or memory pressure
)

// featureRule defines the tier requirement and memory budget for a feature.
type featureRule struct {
	MinTier         Tier   // minimum hardware tier
	MinMemoryMB     uint64 // minimum free memory required (dynamic check)
	DegradeMemoryMB uint64 // if free memory drops below this, degrade
	Priority        int    // lower = more important; determines degradation order
	OSConstraint    string // empty = any; "linux" / "darwin_only" etc.
}

// featureRules is the single source of truth for all feature gating decisions.
// 所有门控规则对应架构文档 ROADMAP.md §4.7 + state.yaml §thresholds。
var featureRules = map[Feature]featureRule{
	FeatureLocalInference: {MinTier: Tier1, MinMemoryMB: 2048, DegradeMemoryMB: 3072, Priority: 20, OSConstraint: ""},
	FeatureLocalEmbedding: {MinTier: Tier0, MinMemoryMB: 256, DegradeMemoryMB: 512, Priority: 10, OSConstraint: ""},
	FeatureLocalSTT:       {MinTier: Tier0, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 12, OSConstraint: ""},
	FeatureQLoRA:          {MinTier: Tier1, MinMemoryMB: 4096, DegradeMemoryMB: 6144, Priority: 50, OSConstraint: ""},
	FeaturePRMTraining:    {MinTier: Tier2, MinMemoryMB: 8192, DegradeMemoryMB: 12288, Priority: 60, OSConstraint: ""},
	FeatureL3Sandbox:      {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 30},
	FeatureL2Sandbox:      {MinTier: Tier0, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 5},
	FeatureGraphRAGFull:   {MinTier: Tier1, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 40},
	FeatureSurrealDBCore:  {MinTier: Tier0, MinMemoryMB: 256, DegradeMemoryMB: 512, Priority: 8},
	FeatureLargeLocalLLM:  {MinTier: Tier2, MinMemoryMB: 6144, DegradeMemoryMB: 8192, Priority: 55},
	// 桶 B — 新增规则
	FeatureLogicCollapse:   {MinTier: Tier1, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 42},
	FeatureComputerUseGUI:  {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 38, OSConstraint: "requires_display"},
	FeaturePresidioPII:     {MinTier: Tier1, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 36},
	FeatureWebUI:           {MinTier: Tier1, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 15},
	FeatureActivationSteer: {MinTier: Tier1, MinMemoryMB: 1536, DegradeMemoryMB: 2048, Priority: 48},
}

// FeatureGate provides runtime feature availability checks.
// Combines static hardware tier with dynamic memory pressure from OSMemoryGuard.
type FeatureGate struct {
	probe *HardwareProbe
	guard *OSMemoryGuard

	mu        sync.RWMutex
	states    map[Feature]FeatureState
	overrides map[Feature]FeatureState // manual overrides from admin
}

// NewFeatureGate creates a FeatureGate wired to the hardware probe and memory guard.
func NewFeatureGate(probe *HardwareProbe, guard *OSMemoryGuard) *FeatureGate {
	fg := &FeatureGate{
		probe:     probe,
		guard:     guard,
		states:    make(map[Feature]FeatureState),
		overrides: make(map[Feature]FeatureState),
	}
	fg.reassessAll()
	return fg
}

// State returns the current availability of a feature.
// 调用方在每次尝试使用特性前检查此方法。
func (fg *FeatureGate) State(f Feature) FeatureState {
	fg.mu.RLock()
	defer fg.mu.RUnlock()
	if override, ok := fg.overrides[f]; ok {
		return override
	}
	if state, ok := fg.states[f]; ok {
		return state
	}
	return FeatureDisabled
}

// IsEnabled is a convenience method for the common case.
func (fg *FeatureGate) IsEnabled(f Feature) bool {
	return fg.State(f) != FeatureDisabled
}

// Override allows admin to force-enable or force-disable a feature.
// Set to -1 to clear override.
func (fg *FeatureGate) Override(f Feature, state FeatureState) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if state == FeatureState(-1) {
		delete(fg.overrides, f)
	} else {
		fg.overrides[f] = state
	}
}

// reassessAll computes feature availability based on current hardware and memory.
// Features are evaluated in dependency order: base features first, dependent features after.
func (fg *FeatureGate) reassessAll() {
	fg.mu.Lock()
	defer fg.mu.Unlock()

	availableMB := fg.getAvailableMemoryMB()

	// Topological order: base features first, dependent features after
	ordered := []Feature{
		// Layer 0 — no dependencies
		FeatureL2Sandbox,
		FeatureSurrealDBCore,
		FeatureLocalEmbedding,
		FeatureLocalSTT,
		FeatureLocalInference,
		FeatureWebUI,
		FeaturePresidioPII,
		FeatureComputerUseGUI,
		// Layer 1 — depends on L2 features
		FeatureL3Sandbox,
		FeatureQLoRA,
		FeaturePRMTraining,
		FeatureGraphRAGFull,
		FeatureLogicCollapse, // depends on FeatureL2Sandbox
		// Layer 2 — depends on local inference
		FeatureLargeLocalLLM,   // depends on FeatureLocalInference
		FeatureActivationSteer, // depends on FeatureLocalInference
	}

	for _, feature := range ordered {
		rule, ok := featureRules[feature]
		if !ok {
			continue
		}
		fg.states[feature] = fg.computeState(feature, rule, availableMB)
	}
}

// computeState determines feature state from tier + memory + OS constraints + cross-feature dependencies.
func (fg *FeatureGate) computeState(f Feature, rule featureRule, availableMB uint64) FeatureState { //nolint:gocyclo
	// 1. OS constraint check
	switch rule.OSConstraint {
	case "linux":
		if runtime.GOOS != "linux" {
			return FeatureDisabled
		}
	case "darwin_only":
		if runtime.GOOS != "darwin" {
			return FeatureDisabled
		}
	case "requires_display":
		if !hasDisplay() {
			return FeatureDisabled
		}
	}

	// 2. Cross-feature dependencies — use stateWithOverride (no lock, caller holds mu)
	switch f {
	case FeatureActivationSteer:
		if fg.stateWithOverride(FeatureLocalInference) == FeatureDisabled {
			return FeatureDisabled
		}
	case FeatureLargeLocalLLM:
		if fg.stateWithOverride(FeatureLocalInference) == FeatureDisabled {
			return FeatureDisabled
		}
	case FeatureLogicCollapse:
		if fg.stateWithOverride(FeatureL2Sandbox) == FeatureDisabled {
			return FeatureDisabled
		}
	}

	// 3. Hardware tier insufficient → disabled
	if fg.probe.Tier < rule.MinTier {
		return FeatureDisabled
	}

	// 4. Memory abundance → fully enabled
	if availableMB >= rule.MinMemoryMB {
		return FeatureEnabled
	}

	// 5. Degraded zone
	if availableMB >= rule.DegradeMemoryMB {
		return FeatureDegraded
	}

	// 6. Memory pressure → disabled
	return FeatureDisabled
}

// hasDisplay returns true if the current process has access to a graphical display.
func hasDisplay() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true // GUI is always available
	case "linux":
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	default:
		return false
	}
}

// Reassess updates the probe's available RAM and recomputes feature states.
// Called by OSMemoryGuard when memory pressure changes. The caller is responsible
// for hysteresis; Reassess always recomputes.
func (fg *FeatureGate) Reassess(availableMB uint64) {
	fg.probe.AvailableRAM = availableMB * 1024 * 1024
	fg.reassessAll()
}

// getAvailableMemoryMB estimates current free memory in MB.
func (fg *FeatureGate) getAvailableMemoryMB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	heapMB := m.HeapAlloc / (1024 * 1024)
	if fg.probe.AvailableRAM > heapMB {
		return (fg.probe.AvailableRAM - heapMB) / (1024 * 1024)
	}
	return 0
}

// EnabledFeatures returns the list of currently enabled features.
func (fg *FeatureGate) EnabledFeatures() []Feature {
	fg.mu.RLock()
	defer fg.mu.RUnlock()

	var enabled []Feature
	for f, state := range fg.states {
		if _, overridden := fg.overrides[f]; overridden {
			continue
		}
		if state == FeatureEnabled || state == FeatureDegraded {
			enabled = append(enabled, f)
		}
	}
	return enabled
}

// DegradationOrder returns features sorted by degradation priority (highest first).
// When memory is tight, disable features in this order.
func (fg *FeatureGate) DegradationOrder() []Feature {
	return []Feature{
		FeaturePRMTraining,     // 60: heaviest, disable first
		FeatureLargeLocalLLM,   // 55: 7B+ models
		FeatureQLoRA,           // 50: gradient training
		FeatureActivationSteer, // 48: hidden_state injection
		FeatureLogicCollapse,   // 42: TinyGo compile
		FeatureGraphRAGFull,    // 40: Leiden + KuzuDB
		FeatureComputerUseGUI,  // 38: VLM + screen control
		FeaturePresidioPII,     // 36: NER sidecar
		FeatureL3Sandbox,       // 30: microVM
		FeatureLocalInference,  // 20: local model
		FeatureWebUI,           // 15: Web dashboard
		FeatureLocalSTT,        // 12: 本地 STT（sherpa-onnx）
		FeatureLocalEmbedding,  // 10: embedding model
		FeatureSurrealDBCore,   // 8: 认知轴存储，次于 L2Sandbox 降级
		FeatureL2Sandbox,       // 5: Wasmtime, last to disable
	}
}

// ShouldDegrade returns features that should be degraded given current memory.
func (fg *FeatureGate) ShouldDegrade(availableMB uint64) []Feature {
	fg.mu.RLock()
	defer fg.mu.RUnlock()

	var toDegrade []Feature
	for _, f := range fg.DegradationOrder() {
		rule, ok := featureRules[f]
		if !ok {
			continue
		}
		if availableMB < rule.DegradeMemoryMB {
			toDegrade = append(toDegrade, f)
		}
	}
	return toDegrade
}

// Load returns the current load as (inFlight features, total enabled features).
func (fg *FeatureGate) Load() (int, int) {
	fg.mu.RLock()
	defer fg.mu.RUnlock()

	enabled := 0
	degraded := 0
	for _, state := range fg.states {
		switch state {
		case FeatureEnabled:
			enabled++
		case FeatureDegraded:
			degraded++
		}
	}
	return degraded, enabled + degraded
}

// stateWithOverride returns the effective state, considering overrides.
// Caller must hold fg.mu (read or write lock).
func (fg *FeatureGate) stateWithOverride(f Feature) FeatureState {
	if override, ok := fg.overrides[f]; ok {
		return override
	}
	if state, ok := fg.states[f]; ok {
		return state
	}
	return FeatureDisabled
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TierQLoRAModel returns the recommended QLoRA model size for the current tier.
func TierQLoRAModel(tier Tier) (modelSize string, enabled bool) {
	switch {
	case tier >= Tier2:
		return "7B", true
	case tier >= Tier1:
		return "1-3B", true
	default:
		return "", false
	}
}

// TierLocalModel returns the recommended local model size for the current tier.
func TierLocalModel(tier Tier) (modelID string, enabled bool) {
	switch {
	case tier >= Tier3:
		return "Qwen3-32B-Q4_K_M", true
	case tier >= Tier2:
		return "Qwen3-14B-Q4_K_M", true
	case tier >= Tier1:
		return "Qwen3-8B-Q4_K_M", true
	default:
		return "Qwen3-3B-Q4_K_M", false // available only in local_only mode or manual override
	}
}

// TierSandboxConfig returns the sandbox configuration for the current tier.
func TierSandboxConfig(tier Tier, platform string) (l3Available bool, l3Backend string) {
	switch {
	case tier >= Tier2 && platform == "linux":
		return true, "firecracker"
	case tier >= Tier1 && platform == "darwin":
		return true, "virtualization_framework"
	case tier >= Tier2 && platform == "windows":
		return true, "wsl2"
	default:
		return false, ""
	}
}

// 全局 FeatureGate 单例，启动期由 AutoConfig 初始化。
var globalFeatureGate atomic.Pointer[FeatureGate]

// SetGlobalFeatureGate sets the global feature gate singleton.
func SetGlobalFeatureGate(fg *FeatureGate) {
	globalFeatureGate.Store(fg)
}

// GlobalFeatureGate returns the global feature gate, or nil if not initialized.
func GlobalFeatureGate() *FeatureGate {
	return globalFeatureGate.Load()
}
