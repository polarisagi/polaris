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
	FeatureLocalSTT       Feature = "local_stt"       // M13: sherpa-onnx 本地语音识别（SenseVoice）
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
	// Embedding 档位阶梯（优先级递增，内存压力时从高往低降级）：
	//   FeatureLocalEmbedding  → nomic-embed-text        768-dim  ~512MB  Tier0 ≥256MB
	//   FeatureHQEmbedding     → qwen3-embedding:0.6b   1024-dim  ~1GB   Tier0 ≥3GB
	//   FeatureUltraEmbedding  → qwen3-embedding:4b     2560-dim  ~4GB   Tier1 ≥6GB
	//   FeatureMaxEmbedding    → qwen3-embedding:8b     4096-dim  ~10GB  Tier2 ≥12GB
	FeatureHQEmbedding    Feature = "hq_embedding"    // M1: qwen3-embedding:0.6b（Tier0, ≥3GB free）
	FeatureUltraEmbedding Feature = "ultra_embedding" // M1: qwen3-embedding:4b（Tier1, ≥6GB free）
	FeatureMaxEmbedding   Feature = "max_embedding"   // M1: qwen3-embedding:8b（Tier2, ≥12GB free）

	// STT 档位阶梯（SenseVoice via sherpa-onnx，同一 C struct 路径，仅模型文件与线程数不同）：
	//   FeatureLocalSTT（≥512MB）→ int8 量化 SenseVoice（~87MB，~200MB 运行时，速度优先）
	//   FeatureHQSTT（≥1GB）     → float32 SenseVoice（~170MB，~400MB 运行时，精度优先）
	// 两档均支持 zh/en/ja/ko/yue 多语种。
	//
	// 原始 FeatureLocalSTT 门控阈值 128MB 几乎始终开启（不合理），现提升至 512MB：
	//   ≥512MB 可用 → STT 标准档（int8）
	//   ≥1GB  可用 → STT HQ 档（float32，更高 WER 精度，自动升档）
	//   <512MB     → STT 禁用（2GB VPS 下约 400MB 可用，明确禁用）
	FeatureHQSTT Feature = "hq_stt" // M13: float32 SenseVoice（Tier0, ≥1GB free）

	// TTS 独立门控（之前错误地与 FeatureLocalSTT 共享同一门控）。
	// 模型：Kokoro multi-lang v1.1（82MB，~200MB 运行时，zh+en 双语）。
	// 设为独立门控的原因：TTS 可按需禁用（纯 CLI 场景无需语音输出），
	// 且内存占用独立于 STT（可只开 STT 不开 TTS 以节省内存）。
	FeatureLocalTTS Feature = "local_tts" // M13: 本地 TTS（Kokoro，Tier0, ≥512MB free）
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
		// STT 标准档：int8 SenseVoice（~87MB 模型文件，~200MB 运行时）。
		// 原始阈值 128MB 在 2GB VPS 上几乎始终满足（不合理），提升至 512MB 保证 OS 有足够余量。
		FeatureLocalSTT: {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 12, OSConstraint: ""},
		// STT 高质量档：float32 SenseVoice（~170MB 模型文件，~400MB 运行时，WER 更低）。
		// 开启时自动替换 int8 模型；内存不足则回退到 FeatureLocalSTT int8 档。
		FeatureHQSTT: {MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 17},
		// TTS 独立门控：Kokoro multi-lang v1.1（82MB，~200MB 运行时，zh+en 双语）。
		// 之前错误地与 FeatureLocalSTT 共享门控导致 TTS bug，现分离为独立门控。
		FeatureLocalTTS:    {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 11},
		FeatureQLoRA:       {MinTier: Tier1, MinMemoryMB: 4096, DegradeMemoryMB: 6144, Priority: 50, OSConstraint: ""},
		FeaturePRMTraining: {MinTier: Tier2, MinMemoryMB: 8192, DegradeMemoryMB: 12288, Priority: 60, OSConstraint: ""},
		FeatureL3Sandbox:   {MinTier: Tier0, MinMemoryMB: 512, DegradeMemoryMB: 768, Priority: 30},
		FeatureL2Sandbox:   {MinTier: Tier0, MinMemoryMB: 128, DegradeMemoryMB: 256, Priority: 5},
		// GraphRAGFull/LogicCollapse/DeepRAG 原为 Tier1（基于旧 "rocksdb 需要 ≥16GB" 假设）。
		// rocksdb 已下放到 ≥8GB 自动开启，三个特性的实际内存门槛仅 1GB 空闲，8GB 余量 ~5GB。
		FeatureGraphRAGFull:  {MinTier: Tier0, MinMemoryMB: 1024, DegradeMemoryMB: 1536, Priority: 40},
		FeatureSurrealDBCore: {MinTier: Tier0, MinMemoryMB: 256, DegradeMemoryMB: 512, Priority: 8},
		FeatureLargeLocalLLM: {MinTier: Tier2, MinMemoryMB: 6144, DegradeMemoryMB: 8192, Priority: 55},
		// Embedding 阶梯门控（Priority 越高越先降级，保留低档基础能力）。
		FeatureHQEmbedding:    {MinTier: Tier0, MinMemoryMB: 3072, DegradeMemoryMB: 4096, Priority: 11},   // 0.6b ~1GB
		FeatureUltraEmbedding: {MinTier: Tier1, MinMemoryMB: 6144, DegradeMemoryMB: 8192, Priority: 12},   // 4b  ~4GB
		FeatureMaxEmbedding:   {MinTier: Tier2, MinMemoryMB: 12288, DegradeMemoryMB: 16384, Priority: 13}, // 8b ~10GB
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
		FeatureHQEmbedding, // Embedding 阶梯：门控独立，按内存阈值自动升档
		FeatureUltraEmbedding,
		FeatureMaxEmbedding,
		FeatureLocalSTT,
		FeatureHQSTT,    // 依赖 FeatureLocalSTT（HQ 档必须在标准档之后评估）
		FeatureLocalTTS, // TTS 独立于 STT
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
		FeatureLogicCollapse, // depends on FeatureL3Sandbox（M06 §116，ADR-0026）
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

// Reassess / GetAvailableMemoryMB / EnabledFeatures / DegradationOrder / ShouldDegrade /
// Load / stateWithOverride / AbsDiff / TierQLoRAModel / TierLocalModel / TierSandboxConfig /
// 全局单例(SetGlobalFeatureGate/GlobalFeatureGate) 见 feature_gate_degradation.go（R7 拆分）。
