package observability

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"

	"context"
	"fmt"
	"runtime"
	"sort"
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

// LocalModelUnloader 按名查找 Provider 的最小接口（consumer-side，防止
// internal/observability 反向依赖 internal/llm）。*llm.ProviderRegistry 天然满足。
// 2026-07-04 审计补齐：此前 MemoryPressureCallback 的 DegradationCritical 分支
// 只 Disable Gate + GC + kill 沙箱，从未真正调用 UnloadModel 释放本地模型常驻内存
// （M01 §8.2 明确要求的行为），是任务4的真实缺口，此处补齐。
type LocalModelUnloader interface {
	Get(name string) (protocol.Provider, bool)
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
	// localUnloader 由 llm 侧注入 ProviderRegistry，内存临界时主动卸载本地模型。
	localUnloader LocalModelUnloader
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

// WithLocalModelUnloader 注入本地模型卸载器（consumer-side，2026-07-04 新增）。
func (ac *AutoConfig) WithLocalModelUnloader(u LocalModelUnloader) *AutoConfig {
	ac.localUnloader = u
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
	// LocalEmbeddingDim 实际选用的本地 Embedding 模型输出维度，用于覆盖 SurrealDB HNSW DIMENSION。
	// 0 表示未启用本地 Embedding（远程 API 路径，维度由 cfg.Inference.EmbedderDim 决定）。
	LocalEmbeddingDim int `json:"local_embedding_dim"`

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

	// 本地 Embedding 模型按内存档位自动选取（Ollama 标准 tag，最高档优先）。
	// LocalEmbeddingDim 写入 SurrealDB HNSW DIMENSION，必须与模型输出一致。
	//
	//   档位           Feature               模型                     MTEB  Dim   运行内存  Tier
	//   Max（≥12GB）   FeatureMaxEmbedding   qwen3-embedding:8b       70.58 4096  ~10GB   Tier2
	//   Ultra（≥6GB）  FeatureUltraEmbedding qwen3-embedding:4b       ~68   2560  ~4GB    Tier1
	//   HQ（≥3GB）     FeatureHQEmbedding    qwen3-embedding:0.6b     64.33 1024  ~1GB    Tier0
	//   标准（≥256MB） FeatureLocalEmbedding nomic-embed-text          ~62   768   ~512MB  Tier0
	//   不可用          —                     —（远程 API 路径）        —     —     —
	switch {
	case ac.Gate.State(probe.FeatureMaxEmbedding) != probe.FeatureDisabled:
		c.LocalEmbeddingModel = "qwen3-embedding:8b"
		c.LocalEmbeddingDim = 4096
	case ac.Gate.State(probe.FeatureUltraEmbedding) != probe.FeatureDisabled:
		c.LocalEmbeddingModel = "qwen3-embedding:4b"
		c.LocalEmbeddingDim = 2560
	case ac.Gate.State(probe.FeatureHQEmbedding) != probe.FeatureDisabled:
		c.LocalEmbeddingModel = "qwen3-embedding:0.6b"
		c.LocalEmbeddingDim = 1024
	case ac.Gate.State(probe.FeatureLocalEmbedding) != probe.FeatureDisabled:
		c.LocalEmbeddingModel = "nomic-embed-text"
		c.LocalEmbeddingDim = 768
	default:
		// 本地 Embedding 不可用；SurrealDB 维度及远程 API 路径维度由 cfg.Inference.EmbedderDim 决定。
		c.LocalEmbeddingModel = ""
		c.LocalEmbeddingDim = 0
	}

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
		// Embedding 阶梯
		probe.FeatureHQEmbedding, probe.FeatureUltraEmbedding, probe.FeatureMaxEmbedding,
		// STT/TTS 分级
		probe.FeatureHQSTT, probe.FeatureLocalTTS,
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

// MemoryPressureCallback / unloadLocalModel / RunMemoryWatcher / clearOverrides
// 见 auto_config_pressure.go（R7 拆分）。
// computeTierParameters 见 auto_config_tiers.go（R7 拆分）。
