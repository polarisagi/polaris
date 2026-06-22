package observability

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"fmt"
	"runtime"
	"sort"
	"time"
)

// SandboxController probe.OSMemoryGuard 调用的沙箱管理接口（consumer-side，防包循环）。
type SandboxController interface {
	// DisableNewInstances 禁止启动新 Wasm 沙箱（L1 预警）。
	DisableNewInstances(disable bool)
	// KillIdleSandboxes kill 空闲（无活跃任务）的沙箱（L2 紧急）。
	KillIdleSandboxes(ctx context.Context)
	// KillAllNonCritical kill 全部非关键沙箱，仅保留当前交互任务（L3 临界）。
	KillAllNonCritical(ctx context.Context)
}

// AutoConfig is the startup auto-configuration engine.
// Detects hardware → assigns tier → configures all subsystems → initializes probe.FeatureGate.
// 架构文档: docs/arch/ROADMAP.md §4.7 + spec/state.yaml §thresholds
type AutoConfig struct {
	Probe   *probe.HardwareProbe
	Guard   *probe.OSMemoryGuard
	Gate    *probe.FeatureGate
	TBR     *metrics.TokenBurnRate
	Sandbox SandboxController
	Config  AutoConfigResult
	// surrealPurger 由 storage 侧注入，避免 observability↔storage 包循环。
	surrealPurger func()
}

// WithSandboxController injects the sandbox controller for memory pressure handling.
func (ac *AutoConfig) WithSandboxController(sc SandboxController) *AutoConfig {
	ac.Sandbox = sc
	return ac
}

// WithSurrealPurger injects the SurrealDB memory-purge hook（consumer-side，防包循环）。
func (ac *AutoConfig) WithSurrealPurger(fn func()) *AutoConfig {
	ac.surrealPurger = fn
	return ac
}

// AutoConfigResult is the computed system configuration.
type AutoConfigResult struct {
	Tier           probe.Tier `json:"tier"`
	TierReason     string     `json:"tier_reason"`
	TotalRAMMB     uint64     `json:"total_ram_mb"`
	AvailableRAMMB uint64     `json:"available_ram_mb"`
	CPUCores       int        `json:"cpu_cores"`
	CPUArch        string     `json:"cpu_arch"`
	OS             string     `json:"os"`

	// Inference
	DefaultProvider     string `json:"default_provider"`
	LocalModelAutoLoad  bool   `json:"local_model_auto_load"`
	LocalModelID        string `json:"local_model_id"`
	LocalEmbeddingModel string `json:"local_embedding_model"`

	// Sandbox
	L3SandboxAvailable bool   `json:"l3_sandbox_available"`
	L3SandboxBackend   string `json:"l3_sandbox_backend"`
	ScriptWorkers      int    `json:"script_workers"`

	// Training (M9)
	QLoRAEnabled   bool   `json:"qlora_enabled"`
	QLoRAModelSize string `json:"qlora_model_size"`
	PRMEnabled     bool   `json:"prm_enabled"`

	// Storage engines
	StorageEngines []string `json:"storage_engines"`

	// probe.Feature map
	Features map[probe.Feature]probe.FeatureState `json:"features"`

	// probe.Tier parameters (Bucket C — auto-selected numeric defaults by tier)
	Params probe.TierParameters `json:"params"`

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
	totalRAM, availableRAM := probe.MemoryProbe()

	ac := &AutoConfig{
		Probe: probe.NewHardwareProbe(totalRAM, availableRAM),
		TBR:   metrics.NewTokenBurnRate(),
	}
	ac.Guard = probe.NewOSMemoryGuard(totalRAM / (1024 * 1024))
	ac.Gate = probe.NewFeatureGate(ac.Probe, ac.Guard)
	ac.computeConfig()
	probe.SetGlobalFeatureGate(ac.Gate)

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
	case c.Tier >= probe.Tier2:
		c.DefaultProvider = "deepseek"
	case c.Tier >= probe.Tier0:
		c.DefaultProvider = "deepseek"
	}

	modelID, localOK := probe.TierLocalModel(c.Tier)
	c.LocalModelID = modelID
	c.LocalEmbeddingModel = "BGE-small-Q4_K_M"

	if ac.Gate.State(probe.FeatureLocalInference) == probe.FeatureEnabled {
		c.LocalModelAutoLoad = true
	} else if localOK && ac.Gate.State(probe.FeatureLocalInference) == probe.FeatureDegraded {
		c.LocalModelAutoLoad = true
	} else {
		c.LocalModelAutoLoad = false
	}
}

func (ac *AutoConfig) computeSandboxConfig(c *AutoConfigResult) {
	platform := runtime.GOOS
	c.L3SandboxAvailable, c.L3SandboxBackend = probe.TierSandboxConfig(c.Tier, platform)

	switch {
	case c.Tier >= probe.Tier3:
		c.ScriptWorkers = 16
	case c.Tier >= probe.Tier2:
		c.ScriptWorkers = 12
	case c.Tier >= probe.Tier1:
		c.ScriptWorkers = 8
	default:
		c.ScriptWorkers = 4
	}
}

func (ac *AutoConfig) computeTrainingConfig(c *AutoConfigResult) {
	c.QLoRAModelSize, c.QLoRAEnabled = probe.TierQLoRAModel(ac.Probe.Tier)
	if c.QLoRAEnabled && ac.Gate.State(probe.FeatureQLoRA) == probe.FeatureDisabled {
		c.QLoRAEnabled = false
		c.QLoRAModelSize = ""
	}
	c.PRMEnabled = ac.Gate.IsEnabled(probe.FeaturePRMTraining)
}

func (ac *AutoConfig) computeStorageConfig(c *AutoConfigResult) {
	engines := []string{"sqlite", "surreal"}
	sort.Strings(engines)
	c.StorageEngines = engines
}

func (ac *AutoConfig) computeMemoryBudget(c *AutoConfigResult) {
	totalMB := c.TotalRAMMB
	availableMB := c.AvailableRAMMB

	c.MemoryBudgetDetails = BudgetBreakdown{
		ReservedMB: 1024,
	}

	switch {
	case c.Tier >= probe.Tier3:
		c.MemoryBudgetDetails.AgentRuntimeMB = 4096
		c.MemoryBudgetDetails.LocalModelsMB = 8192
		c.MemoryBudgetDetails.StorageMB = 2048
		c.MemoryBudgetDetails.SandboxMB = 2048
	case c.Tier >= probe.Tier2:
		c.MemoryBudgetDetails.AgentRuntimeMB = 2048
		c.MemoryBudgetDetails.LocalModelsMB = 4096
		c.MemoryBudgetDetails.StorageMB = 1024
		c.MemoryBudgetDetails.SandboxMB = 1024
	case c.Tier >= probe.Tier1:
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
	c.Features = make(map[probe.Feature]probe.FeatureState)
	allFeatures := []probe.Feature{
		probe.FeatureLocalInference, probe.FeatureLocalEmbedding, probe.FeatureLocalSTT, probe.FeatureQLoRA, probe.FeaturePRMTraining,
		probe.FeatureL3Sandbox, probe.FeatureL2Sandbox, probe.FeatureGraphRAGFull,
		probe.FeatureSurrealDBCore, probe.FeatureLargeLocalLLM,
		probe.FeatureLogicCollapse, probe.FeatureComputerUseGUI, probe.FeaturePresidioPII,
		probe.FeatureWebUI, probe.FeatureActivationSteer,
		probe.FeatureOTelExporter, probe.FeatureDeepRAG,
	}
	for _, f := range allFeatures {
		c.Features[f] = ac.Gate.State(f)
	}
}

// Summary returns a human-readable configuration summary for startup logging.
func (ac *AutoConfig) Summary() string {
	c := &ac.Config
	return fmt.Sprintf(
		"AutoConfig: tier=T%d(%s) ram=%dMB(avail=%dMB) cpu=%d arch=%s os=%s provider=%s "+
			"local_model=%s(autoload=%v) qlora=%s(enabled=%v) l3_sandbox=%s(backend=%s) "+
			"script_workers=%d storage=%v",
		c.Tier, c.TierReason, c.TotalRAMMB, c.AvailableRAMMB,
		c.CPUCores, c.CPUArch, c.OS,
		c.DefaultProvider,
		c.LocalModelID, c.LocalModelAutoLoad,
		c.QLoRAModelSize, c.QLoRAEnabled,
		map[bool]string{true: "yes", false: "no"}[c.L3SandboxAvailable], c.L3SandboxBackend,
		c.ScriptWorkers, c.StorageEngines,
	)
}

// MemoryPressureCallback is called by probe.OSMemoryGuard when pressure level changes.
// Hysteresis: 256 MB threshold before reassessing to avoid thrashing.
func (ac *AutoConfig) MemoryPressureCallback(availableMB uint64, level probe.DegradationLevel) {
	prevMB := ac.Gate.GetAvailableMemoryMB()
	if probe.AbsDiff(prevMB, availableMB) < 256 {
		return
	}
	ac.Gate.Reassess(availableMB)

	switch level {
	case probe.DegradationCritical:
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLargeLocalLLM, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLocalInference, probe.FeatureDisabled)
		// 触发 Go GC 尽快归还内存给 OS，降低 OOM 风险
		runtime.GC()
		// 通知 SurrealDB FFI 侧执行内存压缩（best-effort，忽略返回值）
		if ac.surrealPurger != nil {
			ac.surrealPurger()
		}
		// M07 §12 L3：kill 全部非关键沙箱
		if ac.Sandbox != nil {
			ac.Sandbox.KillAllNonCritical(context.Background())
		}
	case probe.DegradationWarning:
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLargeLocalLLM, probe.FeatureDegraded)
		// M07 §12 L2：kill 空闲沙箱
		if ac.Sandbox != nil {
			ac.Sandbox.KillIdleSandboxes(context.Background())
		}
	case probe.DegradationCaution:
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDegraded)
		// M07 §12 L1：禁止启动新 Wasm 实例
		if ac.Sandbox != nil {
			ac.Sandbox.DisableNewInstances(true)
		}
	case probe.DegradationNone:
		ac.clearOverrides()
		if ac.Sandbox != nil {
			ac.Sandbox.DisableNewInstances(false)
		}
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
			availMB := probe.ProbeAvailableMemoryMB()
			level := ac.Guard.CheckAndProtect(availMB)
			ac.MemoryPressureCallback(availMB, level)
		}
	}
}

func (ac *AutoConfig) clearOverrides() {
	for _, f := range []probe.Feature{
		probe.FeatureQLoRA, probe.FeatureLargeLocalLLM, probe.FeatureLocalInference,
		probe.FeaturePRMTraining, probe.FeatureL3Sandbox, probe.FeatureLogicCollapse,
		probe.FeatureActivationSteer, probe.FeatureGraphRAGFull,
	} {
		ac.Gate.Override(f, probe.FeatureState(-1))
	}
}

func (ac *AutoConfig) computeTierParameters(p *probe.TierParameters) {
	switch ac.Probe.Tier {
	case probe.Tier3: // 64GB+
		p.MaxConcurrentDAGNodes = 16
		p.MaxAgents = 12
		p.MaxReplanAttempts = 4
		p.IntentChannelBuffer = 32
		p.EventsChannelBuffer = 128
		p.MemL0CacheMB = 512
		p.GraphMaxDepth = 6
		p.BackfillConcurrency = 4
		p.MaxLogicCollapseConcurrent = 4
		p.SkillPreloadGold = 20
		p.SkillPreloadSilver = 80
		p.SkillPreloadBronze = 200
		p.ScriptWorkerMax = 16
		p.MaxStreamBufferKB = 1024
		p.MaxBlackboardPending = 1024
		p.MaxCoordinationToken = 500000
		p.PipelineConcurrency = 8
		p.GraphRAGLLMDailyBudget = 1000
		p.GraphRAGMaxEntities = 500000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 15
		p.PoolIngest = 12
		p.PoolBackground = 20
		p.PoolEval = 6
		p.PoolCron = 6

	case probe.Tier2: // 24GB+
		p.MaxConcurrentDAGNodes = 12
		p.MaxAgents = 8
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 24
		p.EventsChannelBuffer = 96
		p.MemL0CacheMB = 256
		p.GraphMaxDepth = 5
		p.BackfillConcurrency = 3
		p.MaxLogicCollapseConcurrent = 4
		p.SkillPreloadGold = 15
		p.SkillPreloadSilver = 60
		p.SkillPreloadBronze = 150
		p.ScriptWorkerMax = 12
		p.MaxStreamBufferKB = 1024
		p.MaxBlackboardPending = 512
		p.MaxCoordinationToken = 350000
		p.PipelineConcurrency = 6
		p.GraphRAGLLMDailyBudget = 500
		p.GraphRAGMaxEntities = 200000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 10
		p.PoolIngest = 8
		p.PoolBackground = 15
		p.PoolEval = 4
		p.PoolCron = 4

	case probe.Tier1: // 16GB
		p.MaxConcurrentDAGNodes = 8
		p.MaxAgents = 5
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 16
		p.EventsChannelBuffer = 64
		p.MemL0CacheMB = 160
		p.GraphMaxDepth = 4
		p.BackfillConcurrency = 2
		p.MaxLogicCollapseConcurrent = 2
		p.SkillPreloadGold = 10
		p.SkillPreloadSilver = 40
		p.SkillPreloadBronze = 100
		p.ScriptWorkerMax = 8
		p.MaxStreamBufferKB = 512
		p.MaxBlackboardPending = 256
		p.MaxCoordinationToken = 200000
		p.PipelineConcurrency = 4
		p.GraphRAGLLMDailyBudget = 200
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 20
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2

	default: // probe.Tier0 8GB
		p.MaxConcurrentDAGNodes = 4
		p.MaxAgents = 3
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 8
		p.EventsChannelBuffer = 32
		p.MemL0CacheMB = 80
		p.GraphMaxDepth = 3
		p.BackfillConcurrency = 1
		p.MaxLogicCollapseConcurrent = 1 // LogicCollapse 在 probe.Tier0 启用，单并发限制编译期内存峰值
		p.SkillPreloadGold = 5
		p.SkillPreloadSilver = 20
		p.SkillPreloadBronze = 25
		p.ScriptWorkerMax = 4
		p.MaxStreamBufferKB = 256
		p.MaxBlackboardPending = 128
		p.MaxCoordinationToken = 100000
		p.PipelineConcurrency = 2
		p.GraphRAGLLMDailyBudget = 200
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 10
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2
	}
}
