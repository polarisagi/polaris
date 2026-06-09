package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/configs"
)

var BuildVersion = "dev"

type Config struct {
	System        SystemConfig        `toml:"system"`
	Download      DownloadConfig      `toml:"download"`
	Inference     InferenceConfig     `toml:"inference"`
	Storage       StorageConfig       `toml:"storage"`
	Observability ObservabilityConfig `toml:"observability"`
	Agent         AgentConfig         `toml:"agent"`
	Orchestrator  OrchestratorConfig  `toml:"orchestrator"`
	SelfImprove   SelfImproveConfig   `toml:"self_improve"`
	Knowledge     KnowledgeConfig     `toml:"knowledge"`
	Policy        PolicyConfig        `toml:"policy"`
	Eval          EvalConfig          `toml:"eval"`
	Interface     InterfaceConfig     `toml:"interface"`
	Thresholds    Thresholds          `toml:"-"`
}

// DownloadConfig 控制文件下载行为，包括中国区 GitHub 加速代理。
type DownloadConfig struct {
	// GithubProxy 控制 GitHub 资源的下载代理策略。
	// 取值：
	//   "auto"                 — 自动探测（默认）：连不上 github.com 时自动切换 ghproxy
	//   "off" / "none"         — 始终直连，禁用代理
	//   "https://ghproxy.net"  — 强制使用指定代理，不再探测
	// 环境变量 POLARIS_GITHUB_PROXY 优先级高于此配置。
	GithubProxy string `toml:"github_proxy"`
}

type SystemConfig struct {
	Tier         int        `toml:"tier"`
	MaxAgents    int        `toml:"max_agents"`
	GoMemLimitMB int        `toml:"go_memlimit_mb"`
	DataDir      string     `toml:"data_dir"`
	Dirs         DirsConfig `toml:"dirs"`
}

// DirsConfig 允许 Operator 将特定子目录挂载到其他磁盘/分区。
// 未设置的字段自动从 DataDir 派生（见 DataLayout.NewDataLayout）。
// 典型场景：logs_dir 指向中央日志盘；db_dir 指向高速 NVMe；workspace_dir 指向 tmpfs。
type DirsConfig struct {
	LogsDir      string `toml:"logs_dir"`      // 覆盖 DataDir/logs
	DBDir        string `toml:"db_dir"`        // 覆盖 DataDir/data（数据库文件）
	WorkspaceDir string `toml:"workspace_dir"` // 覆盖 DataDir/workspace（Agent VFS 沙箱）
	ModelsDir    string `toml:"models_dir"`    // 覆盖 DataDir/models（AI 模型文件）
}

type InferenceConfig struct {
	DefaultProvider   string      `toml:"default_provider"`
	ReasoningProvider string      `toml:"reasoning_provider"`
	StructuredOutput  string      `toml:"structured_output"`
	EmbedderDim       int         `toml:"embedder_dim"` // vector dimension; changes on local_only toggle
	Cache             CacheConfig `toml:"cache"`
	STT               STTConfig   `toml:"stt"`
	TTS               TTSConfig   `toml:"tts"`
}

type STTConfig struct {
	SherpaVersion      string `toml:"sherpa_version"`
	SenseVoiceModelURL string `toml:"sense_voice_model_url"`
	PunctModelURL      string `toml:"punct_model_url"`
}

// TTSConfig 本地 TTS（sherpa-onnx）模型下载配置。
// 模型文件下载后存放在 data_dir/models/tts/。
type TTSConfig struct {
	// SherpaVersion 与 STT 共用同一 sherpa-onnx 版本（共享动态库）。
	// 留空时自动复用 inference.stt.sherpa_version。
	SherpaVersion string `toml:"sherpa_version"`
	// ModelURL sherpa-onnx TTS 模型 tar.bz2 下载地址（GitHub Releases）。
	// 留空表示不启用本地 TTS，继续使用 edge-tts 云端 API。
	ModelURL string `toml:"model_url"`
	// TokensURL 词表文件单独下载地址（部分模型将 tokens.txt 独立发布）。
	// 留空时假设 model URL 的归档中已包含 tokens.txt。
	TokensURL string `toml:"tokens_url"`
}

type CacheConfig struct {
	Enabled bool   `toml:"enabled"`
	Backend string `toml:"backend"`
}

type StorageConfig struct {
	Engines map[string]string `toml:"engines"`
}

type ObservabilityConfig struct {
	Traces  TraceConfig  `toml:"traces"`
	Metrics MetricConfig `toml:"metrics"`
	Logs    LogConfig    `toml:"logs"`
}

type TraceConfig struct {
	Enabled bool    `toml:"enabled"`
	Sampler float64 `toml:"sampler"`
}

type MetricConfig struct {
	Enabled bool `toml:"enabled"`
}

type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

type AgentConfig struct {
	Kernel KernelConfig `toml:"kernel"`
	Memory MemoryConfig `toml:"memory"`
	Skill  SkillConfig  `toml:"skill"`
}

type KernelConfig struct {
	StateMachine             string  `toml:"state_machine"`
	DefaultSurpriseThreshold float64 `toml:"default_surprise_threshold"`
}

type MemoryConfig struct {
	Layers        []string `toml:"layers"`
	Consolidation string   `toml:"consolidation"`
}

type SkillConfig struct {
	BuiltinPath                string `toml:"builtin_path"`
	MaxLogicCollapseConcurrent int    `toml:"max_logic_collapse_concurrent"`
}

type OrchestratorConfig struct {
	Mode     string `toml:"mode"`
	Protocol string `toml:"protocol"`
}

type SelfImproveConfig struct {
	Gradient       bool                `toml:"gradient"`
	AutoCurriculum bool                `toml:"auto_curriculum"`
	LogicCollapse  LogicCollapseConfig `toml:"logic_collapse"`
}

type LogicCollapseConfig struct {
	Enabled              bool `toml:"enabled"`
	MinSuccessForTrigger int  `toml:"min_success_for_trigger"`
}

type KnowledgeConfig struct {
	RAG RAGConfig `toml:"rag"`
}

type RAGConfig struct {
	Mode     string `toml:"mode"`
	GraphRAG string `toml:"graphrag"`
}

type PolicyConfig struct {
	Engine       string `toml:"engine"`
	DefaultBlock bool   `toml:"default_block"`
}

type EvalConfig struct {
	CIGate       bool `toml:"ci_gate"`
	ShadowDeploy bool `toml:"shadow_deploy"`
}

type InterfaceConfig struct {
	Host      string `toml:"host"`
	Port      int    `toml:"port"`
	CLI       bool   `toml:"cli"`
	HTTP      bool   `toml:"http"`
	GRPC      bool   `toml:"grpc"`
	WebSocket bool   `toml:"websocket"`
}

func loadModuleTOML(modulePath string, target interface{}) error {
	if _, err := os.Stat(modulePath); os.IsNotExist(err) {
		return nil
	}
	data, err := os.ReadFile(modulePath)
	if err != nil {
		slog.Error("polaris: failed to read threshold override", "file", modulePath, "err", err)
		return err
	}
	if err := toml.Unmarshal(data, target); err != nil {
		slog.Error("polaris: failed to parse threshold override", "file", modulePath, "err", err)
		return err
	}
	slog.Info("polaris: threshold override loaded", "file", modulePath)
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback to embedded configs
		data, err = configs.FS.ReadFile("defaults.toml")
		if err != nil {
			return nil, err
		}

		// Attempt to export the defaults to the path for future edits
		if errMkdir := os.MkdirAll(filepath.Dir(path), 0755); errMkdir == nil {
			os.WriteFile(path, data, 0600) //nolint:errcheck
		}
	}
	cfg := &Config{Thresholds: DefaultThresholds()}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 对边界非法值做 Fail-Fast 校验，防止明显错误配置在运行期才暴露 panic。
// 未填写的字段（零值）代表"使用系统默认"，不视为错误。
func (c *Config) Validate() error {
	if c.System.Tier < 0 || c.System.Tier > 3 {
		return fmt.Errorf("config: system.tier must be 0-3, got %d", c.System.Tier)
	}
	// go_memlimit_mb 为 0 代表不设 GOMEMLIMIT（由运行时自动管理），合法。
	// 非零时要求最低 64MB，低于此值会导致频繁 GC 甚至 OOM。
	if c.System.GoMemLimitMB != 0 && c.System.GoMemLimitMB < 64 {
		return fmt.Errorf("config: system.go_memlimit_mb must be >= 64 when set, got %d", c.System.GoMemLimitMB)
	}
	return nil
}

func LoadThresholds(dataDir string) (*Thresholds, error) {
	t := DefaultThresholds()
	configDir := os.Getenv("POLARIS_THRESHOLDS_DIR")
	if configDir == "" {
		configDir = filepath.Join(dataDir, "config")
	}

	modules := map[string]interface{}{
		"m1_router.toml":        &t.M1Router,
		"m2_storage.toml":       &t.M2Storage,
		"m3_observability.toml": &t.M3Observability,
		"m4_kernel.toml":        &t.M4Kernel,
		"m5_memory.toml":        &t.M5Memory,
		"m6_skill.toml":         &t.M6Skill,
		"m7_tool.toml":          &t.M7Tool,
		"m8_orchestrator.toml":  &t.M8Orchestrator,
		"m9_self_improve.toml":  &t.M9SelfImprove,
		"m10_knowledge.toml":    &t.M10Knowledge,
		"m11_policy.toml":       &t.M11Policy,
		"m12_eval.toml":         &t.M12Eval,
		"m13_interface.toml":    &t.M13Interface,
	}

	for file, target := range modules {
		if err := loadModuleTOML(filepath.Join(configDir, file), target); err != nil {
			return nil, err
		}
	}

	return &t, nil
}
