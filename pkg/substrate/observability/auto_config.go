package observability

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"time"
)

// AutoConfig is the startup auto-configuration engine.
// Detects hardware → assigns tier → configures all subsystems → initializes FeatureGate.
// 架构文档: docs/arch/ROADMAP.md §4.7 + spec/state.yaml §thresholds
type AutoConfig struct {
	Probe  *HardwareProbe
	Guard  *OSMemoryGuard
	Gate   *FeatureGate
	Config AutoConfigResult
}

// AutoConfigResult is the computed system configuration.
type AutoConfigResult struct {
	Tier           Tier   `json:"tier"`
	TierReason     string `json:"tier_reason"`
	TotalRAMMB     uint64 `json:"total_ram_mb"`
	AvailableRAMMB uint64 `json:"available_ram_mb"`
	CPUCores       int    `json:"cpu_cores"`
	CPUArch        string `json:"cpu_arch"`
	OS             string `json:"os"`

	// Inference
	DefaultProvider     string `json:"default_provider"`
	LocalModelAutoLoad  bool   `json:"local_model_auto_load"`
	LocalModelID        string `json:"local_model_id"`
	LocalEmbeddingModel string `json:"local_embedding_model"`

	// Sandbox
	L3SandboxAvailable bool   `json:"l3_sandbox_available"`
	L3SandboxBackend   string `json:"l3_sandbox_backend"`
	WasmConcurrency    int    `json:"wasm_concurrency"`

	// Training (M9)
	QLoRAEnabled   bool   `json:"qlora_enabled"`
	QLoRAModelSize string `json:"qlora_model_size"`
	PRMEnabled     bool   `json:"prm_enabled"`

	// Storage engines
	StorageEngines []string       `json:"storage_engines"`
	SurrealVecMode SurrealVecMode `json:"surreal_vec_mode"`

	// Feature map
	Features map[Feature]FeatureState `json:"features"`

	// Tier parameters (Bucket C — auto-selected numeric defaults by tier)
	Params TierParameters `json:"params"`

	// Memory budget
	MemoryBudgetMB      uint64          `json:"memory_budget_mb"`
	MemoryBudgetDetails BudgetBreakdown `json:"memory_budget_details"`
}

// BudgetBreakdown shows where memory is allocated.
type BudgetBreakdown struct {
	AgentRuntimeMB uint64 `json:"agent_runtime_mb"`
	LocalModelsMB  uint64 `json:"local_models_mb"`
	StorageMB      uint64 `json:"storage_mb"`
	SandboxMB      uint64 `json:"sandbox_mb"`
	ReservedMB     uint64 `json:"reserved_mb"` // OS + safety margin
}

// NewAutoConfig probes hardware and generates the complete system configuration.
func NewAutoConfig() (*AutoConfig, error) {
	totalRAM, availableRAM := memoryProbe()

	ac := &AutoConfig{
		Probe: NewHardwareProbe(totalRAM, availableRAM),
	}
	ac.Guard = NewOSMemoryGuard(totalRAM / (1024 * 1024))
	ac.Gate = NewFeatureGate(ac.Probe, ac.Guard)
	ac.computeConfig()
	SetGlobalFeatureGate(ac.Gate)

	return ac, nil
}

// computeConfig generates the full configuration based on detected hardware.
func (ac *AutoConfig) computeConfig() {
	p := ac.Probe
	c := &ac.Config

	c.Tier = p.Tier
	c.TierReason = p.TierReason
	c.TotalRAMMB = p.TotalRAM / (1024 * 1024)
	c.AvailableRAMMB = p.AvailableRAM / (1024 * 1024)
	c.CPUCores = p.CPUCores
	c.CPUArch = p.CPUArch
	c.OS = runtime.GOOS

	ac.computeInferenceConfig(c)
	ac.computeSandboxConfig(c)
	ac.computeTrainingConfig(c)
	ac.computeStorageConfig(c)
	ac.computeMemoryBudget(c)
	ac.computeTierParameters(&c.Params)
	ac.computeFeatureMap(c)
}

func (ac *AutoConfig) computeInferenceConfig(c *AutoConfigResult) {
	switch {
	case c.Tier >= Tier2:
		c.DefaultProvider = "deepseek"
	case c.Tier >= Tier0:
		c.DefaultProvider = "deepseek"
	}

	modelID, localOK := TierLocalModel(c.Tier)
	c.LocalModelID = modelID
	c.LocalEmbeddingModel = "BGE-small-Q4_K_M"

	if ac.Gate.State(FeatureLocalInference) == FeatureEnabled {
		c.LocalModelAutoLoad = true
	} else if localOK && ac.Gate.State(FeatureLocalInference) == FeatureDegraded {
		c.LocalModelAutoLoad = true
	} else {
		c.LocalModelAutoLoad = false
	}
}

func (ac *AutoConfig) computeSandboxConfig(c *AutoConfigResult) {
	platform := runtime.GOOS
	c.L3SandboxAvailable, c.L3SandboxBackend = TierSandboxConfig(c.Tier, platform)

	switch {
	case c.Tier >= Tier3:
		c.WasmConcurrency = 16
	case c.Tier >= Tier2:
		c.WasmConcurrency = 12
	case c.Tier >= Tier1:
		c.WasmConcurrency = 8
	default:
		c.WasmConcurrency = 4
	}
}

func (ac *AutoConfig) computeTrainingConfig(c *AutoConfigResult) {
	c.QLoRAModelSize, c.QLoRAEnabled = TierQLoRAModel(ac.Probe.Tier)
	if c.QLoRAEnabled && ac.Gate.State(FeatureQLoRA) == FeatureDisabled {
		c.QLoRAEnabled = false
		c.QLoRAModelSize = ""
	}
	c.PRMEnabled = ac.Gate.IsEnabled(FeaturePRMTraining)
}

// SurrealVecMode 表示向量引擎模式。
type SurrealVecMode int

const (
	SurrealVecBrute SurrealVecMode = 0 // Tier0：暴力余弦扫描
	SurrealVecHNSW  SurrealVecMode = 1 // Tier1+：HNSW 图索引
)

func (ac *AutoConfig) computeStorageConfig(c *AutoConfigResult) {
	engines := []string{"sqlite", "surreal"}
	sort.Strings(engines)
	c.StorageEngines = engines
	c.SurrealVecMode = ac.SurrealVecModeForTier()
}

// SurrealVecModeForTier 返回当前硬件 Tier 对应的向量引擎模式。
// Tier1+ 自动启用 HNSW（O(log N) 查询），Tier0 保持暴力扫描（O(N)，无额外内存开销）。
func (ac *AutoConfig) SurrealVecModeForTier() SurrealVecMode {
	if ac.Gate.IsEnabled(FeatureLocalInference) || ac.Probe.Tier >= Tier1 {
		return SurrealVecHNSW
	}
	return SurrealVecBrute
}

func (ac *AutoConfig) computeMemoryBudget(c *AutoConfigResult) {
	totalMB := c.TotalRAMMB
	availableMB := c.AvailableRAMMB

	c.MemoryBudgetDetails = BudgetBreakdown{
		ReservedMB: 1024,
	}

	switch {
	case c.Tier >= Tier3:
		c.MemoryBudgetDetails.AgentRuntimeMB = 4096
		c.MemoryBudgetDetails.LocalModelsMB = 8192
		c.MemoryBudgetDetails.StorageMB = 2048
		c.MemoryBudgetDetails.SandboxMB = 2048
	case c.Tier >= Tier2:
		c.MemoryBudgetDetails.AgentRuntimeMB = 2048
		c.MemoryBudgetDetails.LocalModelsMB = 4096
		c.MemoryBudgetDetails.StorageMB = 1024
		c.MemoryBudgetDetails.SandboxMB = 1024
	case c.Tier >= Tier1:
		c.MemoryBudgetDetails.AgentRuntimeMB = 1024
		c.MemoryBudgetDetails.LocalModelsMB = 2048
		c.MemoryBudgetDetails.StorageMB = 512
		c.MemoryBudgetDetails.SandboxMB = 768
	default:
		c.MemoryBudgetDetails.AgentRuntimeMB = 512
		c.MemoryBudgetDetails.LocalModelsMB = 0
		c.MemoryBudgetDetails.StorageMB = 384
		c.MemoryBudgetDetails.SandboxMB = 384
	}

	budgetTotal := c.MemoryBudgetDetails.ReservedMB +
		c.MemoryBudgetDetails.AgentRuntimeMB +
		c.MemoryBudgetDetails.LocalModelsMB +
		c.MemoryBudgetDetails.StorageMB +
		c.MemoryBudgetDetails.SandboxMB

	if availableMB < budgetTotal {
		scale := float64(availableMB) / float64(budgetTotal)
		if scale < 1.0 {
			c.MemoryBudgetDetails.AgentRuntimeMB = uint64(float64(c.MemoryBudgetDetails.AgentRuntimeMB) * scale)
			c.MemoryBudgetDetails.LocalModelsMB = uint64(float64(c.MemoryBudgetDetails.LocalModelsMB) * scale)
			c.MemoryBudgetDetails.SandboxMB = uint64(float64(c.MemoryBudgetDetails.SandboxMB) * scale)
		}
	}

	c.MemoryBudgetMB = totalMB
}

func (ac *AutoConfig) computeFeatureMap(c *AutoConfigResult) {
	c.Features = make(map[Feature]FeatureState)
	allFeatures := []Feature{
		FeatureLocalInference, FeatureLocalEmbedding, FeatureLocalSTT, FeatureQLoRA, FeaturePRMTraining,
		FeatureL3Sandbox, FeatureL2Sandbox, FeatureGraphRAGFull,
		FeatureSurrealDBCore, FeatureLargeLocalLLM,
		FeatureLogicCollapse, FeatureComputerUseGUI, FeaturePresidioPII,
		FeatureWebUI, FeatureActivationSteer,
	}
	for _, f := range allFeatures {
		c.Features[f] = ac.Gate.State(f)
	}
}

// Summary returns a human-readable configuration summary for startup logging.
func (ac *AutoConfig) Summary() string {
	c := &ac.Config
	vecMode := "bruteforce"
	if c.SurrealVecMode == SurrealVecHNSW {
		vecMode = "hnsw"
	}
	return fmt.Sprintf(
		"AutoConfig: tier=T%d(%s) ram=%dMB(avail=%dMB) cpu=%d arch=%s os=%s provider=%s "+
			"local_model=%s(autoload=%v) qlora=%s(enabled=%v) l3_sandbox=%s(backend=%s) "+
			"wasm_workers=%d storage=%v vec_mode=%s",
		c.Tier, c.TierReason, c.TotalRAMMB, c.AvailableRAMMB,
		c.CPUCores, c.CPUArch, c.OS,
		c.DefaultProvider,
		c.LocalModelID, c.LocalModelAutoLoad,
		c.QLoRAModelSize, c.QLoRAEnabled,
		map[bool]string{true: "yes", false: "no"}[c.L3SandboxAvailable], c.L3SandboxBackend,
		c.WasmConcurrency, c.StorageEngines, vecMode,
	)
}

// MemoryPressureCallback is called by OSMemoryGuard when pressure level changes.
// Hysteresis: 256 MB threshold before reassessing to avoid thrashing.
func (ac *AutoConfig) MemoryPressureCallback(availableMB uint64, level DegradationLevel) {
	prevMB := ac.Gate.getAvailableMemoryMB()
	if absDiff(prevMB, availableMB) < 256 {
		return
	}
	ac.Gate.Reassess(availableMB)

	switch level {
	case DegradationCritical:
		ac.Gate.Override(FeatureQLoRA, FeatureDisabled)
		ac.Gate.Override(FeatureLargeLocalLLM, FeatureDisabled)
		ac.Gate.Override(FeatureLocalInference, FeatureDisabled)
	case DegradationWarning:
		ac.Gate.Override(FeatureQLoRA, FeatureDisabled)
		ac.Gate.Override(FeatureLargeLocalLLM, FeatureDegraded)
	case DegradationCaution:
		ac.Gate.Override(FeatureQLoRA, FeatureDegraded)
	case DegradationNone:
		ac.clearOverrides()
	}
}

// RunMemoryWatcher polls available system RAM every 5s and drives MemoryPressureCallback.
// Call as a goroutine after AutoConfig is initialized; safe when ac is nil (no-op).
func (ac *AutoConfig) RunMemoryWatcher(ctx context.Context) {
	if ac == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			availMB := ProbeAvailableMemoryMB()
			level := ac.Guard.CheckAndProtect(availMB)
			ac.MemoryPressureCallback(availMB, level)
		}
	}
}

func (ac *AutoConfig) clearOverrides() {
	for _, f := range []Feature{
		FeatureQLoRA, FeatureLargeLocalLLM, FeatureLocalInference,
		FeaturePRMTraining, FeatureL3Sandbox, FeatureLogicCollapse,
		FeatureActivationSteer, FeatureGraphRAGFull,
	} {
		ac.Gate.Override(f, FeatureState(-1))
	}
}
