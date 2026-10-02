package probe

import (
	"os"
	"runtime"
	"sync"
)

// Feature represents a subsystem that can be auto-enabled/disabled based on hardware.
type Feature string

const (
	FeatureLocalInference Feature = "local_inference" // M1: local model loading
	FeatureLocalEmbedding Feature = "local_embedding" // M1: local embedding model
	FeatureQLoRA          Feature = "qlora"           // M9: QLoRA gradient training
	FeaturePRMTraining    Feature = "prm_training"    // M9: PRM trainer worker
	FeatureL3Sandbox      Feature = "l3_sandbox"      // M7: microVM sandbox (Firecracker/VZ)
	FeatureL2Sandbox      Feature = "l2_sandbox"      // M7: Wasmtime sandbox
	FeatureGraphRAGFull   Feature = "graphrag_full"   // M10: Leiden + KuzuDB + LLM community summary
	FeatureSurrealDBCore  Feature = "surrealdb_core"  // M2: SurrealDB-Core 认知轴 (KV+HNSW+BM25+图，CGO-Free FFI)
	FeatureLargeLocalLLM  Feature = "large_local_llm" // M1: 7B+ local model
	// 桶 B — 新增 5 个特性（原为 Tier 硬编码，现自动检测）
	FeatureLogicCollapse       Feature = "logic_collapse"        // M6/M9: System 2→System 1 distillation, TinyGo compile
	FeatureComputerUseGUI      Feature = "computer_use_gui"      // M7: GUI automation (VLM + screen control)
	FeatureVisionDisplayServer Feature = "vision_display_server" // [Task 3] LAM StreamingActionBus Xvfb Backend
	FeaturePresidioPII         Feature = "presidio_pii"          // M11: Microsoft Presidio NER sidecar for PII detection
	FeatureWebUI               Feature = "web_ui"                // M13: go:embed HTMX Web dashboard
	FeatureActivationSteer     Feature = "activation_steer"      // M9: Activation Steering (hidden_state injection)
	FeatureOTelExporter        Feature = "otel_exporter"         // M3: OTel SDK Prometheus exporter（Tier 1+）
	FeatureDeepRAG             Feature = "deep_rag"              // M10: 三阶段深度 RAG（Tier 0+，≥8GB；依赖 rocksdb 持久化，自动升级后索引可落盘）

	// STT/TTS 不在此门控：ADR-0107 起"是否支持"由稳定硬件画像（总内存/逻辑核/arch，
	// internal/llm/audiorun.AudioSupport）判定，空闲内存只决定"此刻能否加载"。
	// 原 FeatureLocalSTT/FeatureHQSTT/FeatureLocalTTS 按瞬时空闲内存自动升降档，
	// 会让同一台机器的语音能力随后台负载来回跳变，且 HQ 档会让首次使用下载 886MB 的 fp32 模型。
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

// getFeatureRules 返回特性门控规则表（只读，进程内只构建一次）。
// 所有门控规则对应架构文档 ROADMAP.md §4.7 + state.yaml §thresholds。
// 使用 sync.OnceValue 替代包级 var map：map 内容初始化后只读，不属于可变全局状态。
var getFeatureRules = sync.OnceValue(func() map[Feature]featureRule {
	return map[Feature]featureRule{
		FeatureLocalInference: {MinTier: Tier1, MinMemoryMB: 2048, DegradeMemoryMB: 3072, Priority: 20, OSConstraint: ""},
		FeatureLocalEmbedding: {MinTier: Tier0, MinMemoryMB: 256, DegradeMemoryMB: 512, Priority: 10, OSConstraint: ""},
		FeatureQLoRA:          {MinTier: Tier1, MinMemoryMB: 4096, DegradeMemoryMB: 6144, Priority: 50, OSConstraint: ""},
		FeaturePRMTraining:    {MinTier: Tier2, MinMemoryMB: 8192, DegradeMemoryMB: 12288, Priority: 60, OSConstraint: ""},
		FeatureL3Sandbox:      {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 30},
		FeatureL2Sandbox:      {MinTier: Tier0, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 5},
		// GraphRAGFull/LogicCollapse/DeepRAG 原为 Tier1（基于旧 "rocksdb 需要 ≥16GB" 假设）。
		// rocksdb 已下放到 ≥8GB 自动开启，三个特性的实际内存门槛仅 1GB 空闲，8GB 余量 ~5GB。
		FeatureGraphRAGFull:  {MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 40},
		FeatureSurrealDBCore: {MinTier: Tier0, MinMemoryMB: 256, DegradeMemoryMB: 512, Priority: 8},
		FeatureLargeLocalLLM: {MinTier: Tier2, MinMemoryMB: 6144, DegradeMemoryMB: 8192, Priority: 55},
		// 桶 B — 新增规则
		FeatureLogicCollapse:       {MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 42},
		FeatureComputerUseGUI:      {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 38, OSConstraint: "requires_display"},
		FeatureVisionDisplayServer: {MinTier: Tier1, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 39, OSConstraint: "linux"},
		FeaturePresidioPII:         {MinTier: Tier1, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 36},
		FeatureWebUI:               {MinTier: Tier1, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 15},
		FeatureActivationSteer:     {MinTier: Tier1, MinMemoryMB: 1536, DegradeMemoryMB: 2048, Priority: 48},
		FeatureDeepRAG:             {MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 45},
		FeatureOTelExporter:        {MinTier: Tier1, MinMemoryMB: 64, DegradeMemoryMB: 128, Priority: 18},
	}
})

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

// HardwareTier returns the underlying hardware tier.
func (fg *FeatureGate) HardwareTier() Tier {
	return fg.probe.Tier
}

// TotalRAM 返回启动时探测的物理总内存（字节）。
func (fg *FeatureGate) TotalRAM() uint64 {
	if fg.probe == nil {
		return 0
	}
	return fg.probe.TotalRAM
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

	availableMB := fg.GetAvailableMemoryMB()

	// Topological order: base features first, dependent features after
	ordered := []Feature{
		// Layer 0 — no dependencies
		FeatureL2Sandbox,
		FeatureSurrealDBCore,
		FeatureLocalEmbedding,
		FeatureLocalInference,
		FeatureWebUI,
		FeaturePresidioPII,
		FeatureComputerUseGUI,
		FeatureVisionDisplayServer,
		// Layer 1 — depends on L2 features
		FeatureL3Sandbox,
		FeatureQLoRA,
		FeaturePRMTraining,
		FeatureGraphRAGFull,
		FeatureDeepRAG,
		FeatureLogicCollapse, // depends on FeatureL3Sandbox（M06 §2.2，ADR-0008）
		// Layer 2 — depends on local inference
		FeatureLargeLocalLLM,   // depends on FeatureLocalInference
		FeatureActivationSteer, // depends on FeatureLocalInference
		// Layer 3 — observability exporters (no cross-feature dependency)
		FeatureOTelExporter,
	}

	for _, feature := range ordered {
		rule, ok := getFeatureRules()[feature]
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
		if fg.stateWithOverride(FeatureL3Sandbox) == FeatureDisabled {
			return FeatureDisabled
		}
	}

	// 3. Hardware tier insufficient → disabled
	if fg.probe.Tier < rule.MinTier {
		return FeatureDisabled
	}

	// 4. Memory abundance → fully enabled
	// 规则语义（featureRule 字段注释）：DegradeMemoryMB > MinMemoryMB，
	// ≥Degrade 全量、[Min, Degrade) 降级、<Min 禁用。此前先判 Min 使降级区间不可达（GR-1.2-001）。
	if availableMB >= rule.DegradeMemoryMB {
		return FeatureEnabled
	}

	// 5. Degraded zone
	if availableMB >= rule.MinMemoryMB {
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

// Reassess / GetAvailableMemoryMB / EnabledFeatures / DegradationOrder / ShouldDegrade /
// Load / stateWithOverride / AbsDiff / TierQLoRAModel / TierLocalModel / TierSandboxConfig /
// 全局单例(SetGlobalFeatureGate/GlobalFeatureGate) 见 feature_gate_degradation.go（R7 拆分）。
