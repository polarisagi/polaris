package probe

import (
	"runtime"
	"sync/atomic"
)

// ============================================================================
// 内存重估 + 降级顺序 + Tier 静态推荐配置 + 全局单例（R7 拆分自 feature_gate.go）。
// Feature 常量/规则表/FeatureGate 结构体/State 判定见 feature_gate.go。
// ============================================================================

// Reassess updates the probe's available RAM and recomputes feature states.
// Called by OSMemoryGuard when memory pressure changes. The caller is responsible
// for hysteresis; Reassess always recomputes.
func (fg *FeatureGate) Reassess(availableMB uint64) {
	fg.probe.AvailableRAM = availableMB * 1024 * 1024
	fg.reassessAll()
}

// GetAvailableMemoryMB estimates current free memory in MB.
// fg.probe.AvailableRAM 以字节存储（来自 probeOSMemory），减去运行时堆占用后换算为 MB。
func (fg *FeatureGate) GetAvailableMemoryMB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	heapBytes := m.HeapAlloc // 单位：字节，与 AvailableRAM 一致
	if fg.probe.AvailableRAM > heapBytes {
		return (fg.probe.AvailableRAM - heapBytes) / (1024 * 1024)
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
		FeaturePRMTraining,         // 60: heaviest, disable first
		FeatureLargeLocalLLM,       // 55: 7B+ models
		FeatureQLoRA,               // 50: gradient training
		FeatureActivationSteer,     // 48: hidden_state injection
		FeatureDeepRAG,             // 45: Three-stage Deep RAG
		FeatureLogicCollapse,       // 42: TinyGo compile
		FeatureGraphRAGFull,        // 40: Leiden + KuzuDB
		FeatureComputerUseGUI,      // 38: VLM + screen control
		FeatureVisionDisplayServer, // 39: LAM Linux Xvfb backend
		FeaturePresidioPII,         // 36: NER sidecar
		FeatureL3Sandbox,           // 30: microVM
		FeatureLocalInference,      // 20: local model
		FeatureOTelExporter,        // 18: OTel exporter
		FeatureWebUI,               // 15: Web dashboard
		FeatureHQSTT,               // 17: float32 SenseVoice（优先降为 int8 标准档，再禁 STT）
		FeatureMaxEmbedding,        // 13: qwen3-embedding:8b（Tier2 ≥12GB free，先降级）
		FeatureUltraEmbedding,      // 12: qwen3-embedding:4b（Tier1 ≥6GB free）
		FeatureLocalSTT,            // 12: int8 SenseVoice（标准 STT，次于 HQ 档降级）
		FeatureHQEmbedding,         // 11: qwen3-embedding:0.6b（Tier0 ≥3GB free）
		FeatureLocalTTS,            // 11: Kokoro TTS（与 HQ Embedding 同档降级）
		FeatureLocalEmbedding,      // 10: nomic-embed-text（基础，最后降级）
		FeatureSurrealDBCore,       // 8: 认知轴存储，次于 L2Sandbox 降级
		FeatureL2Sandbox,           // 5: Wasmtime, last to disable
	}
}

// ShouldDegrade returns features that should be degraded given current memory.
func (fg *FeatureGate) ShouldDegrade(availableMB uint64) []Feature {
	fg.mu.RLock()
	defer fg.mu.RUnlock()

	var toDegrade []Feature
	for _, f := range fg.DegradationOrder() {
		rule, ok := getFeatureRules()[f]
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

func AbsDiff(a, b uint64) uint64 {
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
// l3Backend 取值：
//   - "firecracker"              Tier2+ Linux：microVM 强隔离
//   - "virtualization_framework" Tier1+ macOS：Apple Virtualization Framework
//   - "wsl2"                     Tier2+ Windows：WSL2 容器
//   - "native"                   Tier1 Linux：bwrap + Linux 命名空间隔离（无需 Firecracker）
func TierSandboxConfig(tier Tier, platform string) (l3Available bool, l3Backend string) {
	switch {
	case tier >= Tier2 && platform == "linux":
		return true, "firecracker"
	case tier >= Tier1 && platform == "darwin":
		return true, "virtualization_framework"
	case tier >= Tier2 && platform == "windows":
		return true, "wsl2"
	case tier >= Tier1 && platform == "linux":
		// Tier1 Linux：使用 bwrap + 命名空间隔离（比 Firecracker 轻量，满足最低安全要求）
		return true, "native"
	default:
		return false, ""
	}
}

// 全局 FeatureGate 单例，启动期由 AutoConfig 初始化。
//
// 是否收敛为依赖注入属架构级改动，不在本次 R7 行数拆分范围内处理。
//
//nolint:gochecknoglobals // 迁移前即存在的全局单例（被原文件 new-from-rev 豁免掩盖），非本次拆分引入；
var globalFeatureGate atomic.Pointer[FeatureGate]

// SetGlobalFeatureGate sets the global feature gate singleton.
func SetGlobalFeatureGate(fg *FeatureGate) {
	globalFeatureGate.Store(fg)
}

// GlobalFeatureGate returns the global feature gate, or nil if not initialized.
func GlobalFeatureGate() *FeatureGate {
	return globalFeatureGate.Load()
}
