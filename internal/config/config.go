package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/apperr"
)

var BuildVersion = "dev"

type Config struct {
	System        SystemConfig        `toml:"system"`
	Download      DownloadConfig      `toml:"download"`
	Inference     InferenceConfig     `toml:"inference"`
	Embedding     EmbeddingConfig     `toml:"embedding"`
	Cognition     CognitionConfig     `toml:"cognition"`
	Storage       StorageConfig       `toml:"storage"`
	Observability ObservabilityConfig `toml:"observability"`
	Agent         AgentConfig         `toml:"agent"`
	Orchestrator  OrchestratorConfig  `toml:"orchestrator"`
	SelfImprove   SelfImproveConfig   `toml:"self_improve"`
	Knowledge     KnowledgeConfig     `toml:"knowledge"`
	Policy        PolicyConfig        `toml:"policy"`
	Eval          EvalConfig          `toml:"eval"`
	Interface     InterfaceConfig     `toml:"interface"`
	Compressor    CompressorConfig    `toml:"compressor"`
	Sandbox       SandboxConfig       `toml:"sandbox"`
	Thresholds    Thresholds          `toml:"-"`
}

// CognitionConfig SurrealDB 认知存储后端配置（ADR-0010）。
type CognitionConfig struct {
	// SurrealBackend 后端选择：
	//   "mem"     — kv-mem 默认，进程重启数据丢失，由 SQLite Outbox 投影恢复；256MB+ 可用，含 VPS。
	//   "rocksdb" — kv-rocksdb 持久化，推荐大内存服务器；SurrealDBPath 不可为空。
	SurrealBackend string `toml:"surreal_backend"`
	// SurrealDBPath kv-rocksdb 后端数据库持久化路径；kv-mem 时忽略。
	SurrealDBPath string `toml:"surreal_db_path"`
	// SurrealWorkerThreads Tokio 运行时工作线程数；0 = auto（min(CPU, 4)）。
	// VPS 建议设 2 以节省内存（约 30-50MB）；大内存服务器设 0 自动探测。
	SurrealWorkerThreads int `toml:"surreal_worker_threads"`
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
	Tier                 int                    `toml:"tier"`
	MaxAgents            int                    `toml:"max_agents"`
	GoMemLimitMB         int                    `toml:"go_memlimit_mb"`
	DataDir              string                 `toml:"data_dir"`
	Dirs                 DirsConfig             `toml:"dirs"`
	ResourceGovernor     ResourceGovernorConfig `toml:"resource_governor"`
	DataEncryptionKey    []byte                 `toml:"data_encryption_key"`
	EgressAllowedDomains []string               `toml:"egress_allowed_domains"`
}

type ResourceGovernorConfig struct {
	MemL1FreeMB int     `toml:"mem_l1_free_mb"`
	MemL2FreeMB int     `toml:"mem_l2_free_mb"`
	MemL3FreeMB int     `toml:"mem_l3_free_mb"`
	CPUL1Pct    float64 `toml:"cpu_l1_pct"`
	CPUL2Pct    float64 `toml:"cpu_l2_pct"`
}

// DirsConfig 允许 Operator 将特定子目录挂载到其他磁盘/分区。
// 未设置的字段自动从 DataDir 派生（见 DataLayout.NewDataLayout）。
// 典型场景：logs_dir 指向中央日志盘；db_dir 指向高速 NVMe；workspace_dir 指向 tmpfs。
type DirsConfig struct {
	LogsDir      string `toml:"logs_dir"`      // 覆盖 DataDir/logs
	DBDir        string `toml:"db_dir"`        // 覆盖 DataDir/data（数据库文件）
	WorkspaceDir string `toml:"workspace_dir"` // 覆盖 DataDir/workspace（Agent VFS 沙箱）
	ModelsDir    string `toml:"models_dir"`    // 覆盖 DataDir/models（AI 模型文件）
	BinDir       string `toml:"bin_dir"`       // 覆盖 DataDir/bin（二进制依赖等，如 ollama）
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

// EmbeddingConfig 向量化服务配置。
// BaseURL 留空 = 禁用 Tier 2（降级到词元重叠 Tier 1）。
// 兼容任何 OpenAI /v1/embeddings 兼容端点（DeepSeek-Embed / OpenAI / Jina 等）。
type EmbeddingConfig struct {
	BaseURL   string  `toml:"base_url"`             // 例: "https://api.deepseek.com/v1"
	Model     string  `toml:"model"`                // 例: "deepseek-embed"
	APIKey    string  `toml:"api_key"`              // 空 → 读 POLARIS_EMBEDDING_API_KEY 环境变量
	Threshold float64 `toml:"similarity_threshold"` // 余弦阈值，默认 0.60
}

type STTConfig struct {
	SherpaVersion string `toml:"sherpa_version"`
	// SenseVoiceModelURL float32 高质量模型（~170MB，FeatureHQSTT 开启时使用，WER 更低）。
	SenseVoiceModelURL string `toml:"sense_voice_model_url"`
	// SenseVoiceModelURLStd int8 量化标准模型（~87MB，FeatureLocalSTT 开启但 FeatureHQSTT 未开启时使用）。
	// 空字符串则回退到 SenseVoiceModelURL（向后兼容旧配置）。
	SenseVoiceModelURLStd string `toml:"sense_voice_model_url_std"`
	PunctModelURL         string `toml:"punct_model_url"`
}

// TTSConfig TTS 引擎配置。支持三种 provider：
//   - ""/"sherpa" 本地 sherpa-onnx Kokoro 模型（离线，无网络依赖）
//   - "edge"      Microsoft Edge TTS WebSocket（免费无密钥，中国大陆可用）
//   - "http"      外部 HTTP sidecar（CosyVoice 2 / Qwen3-TTS 等 GPU 推理服务）
type TTSConfig struct {
	// Provider 指定 TTS 引擎类型：""/"sherpa" | "edge" | "http"。
	// 留空等价于 "sherpa"（向后兼容）。
	Provider string `toml:"provider"`

	// ── sherpa provider 专属 ─────────────────────────────────────────────────

	// SherpaVersion 与 STT 共用同一 sherpa-onnx 版本（共享动态库）。
	// 留空时自动复用 llm.stt.sherpa_version。
	SherpaVersion string `toml:"sherpa_version"`
	// ModelURL sherpa-onnx TTS 模型 tar.bz2 下载地址（GitHub Releases）。
	// 留空时 sherpa provider 不启动。
	ModelURL string `toml:"model_url"`
	// TokensURL 词表文件单独下载地址（部分模型将 tokens.txt 独立发布）。
	// 留空时假设 model URL 的归档中已包含 tokens.txt。
	TokensURL string `toml:"tokens_url"`

	// ── edge provider 专属 ──────────────────────────────────────────────────

	// EdgeVoice Microsoft Edge TTS 声线名称。
	// 留空时默认 zh-CN-XiaoxiaoNeural（晓晓，中文女声，音质最佳）。
	// 其他优质中文声线：zh-CN-YunxiNeural（云希，男）/ zh-CN-XiaoYiNeural（晓伊）。
	EdgeVoice string `toml:"edge_voice"`

	// ── http provider 专属 ──────────────────────────────────────────────────

	// HTTPEndpoint 外部 TTS sidecar 的 HTTP 地址，如 "http://127.0.0.1:8000/tts"。
	// provider="http" 时必填。
	HTTPEndpoint string `toml:"http_endpoint"`
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
	WebSearchEngine            string `toml:"web_search_engine"`
	WebSearchAPIKey            string `toml:"web_search_api_key"`
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
	// HardConstraintsPath 硬约束 Cedar 策略文件的磁盘路径；空 = 使用二进制内置 embed 默认策略。
	// 设置后在进程启动时从磁盘加载，支持不重新编译替换策略（运营商自定义场景）。
	HardConstraintsPath string `toml:"hard_constraints_path"`
	// SoftConstraintsPath 软约束 Cedar 策略文件的磁盘路径；空 = 使用二进制内置 embed 默认策略。
	// 软约束可热更新：调用 Gate.ReloadCedarPoliciesFromDisk 无需重启进程。
	SoftConstraintsPath string `toml:"soft_constraints_path"`
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

// SandboxConfig 原生进程沙箱配置（bash / run_command 工具使用）。
// 对齐 Claude Code 三平台策略：macOS Seatbelt / Linux bubblewrap / Windows WSL2。
type SandboxConfig struct {
	// Enabled 是否启用平台原生进程隔离。
	// false = 仅环境变量清理 + workDir 限制（调试模式，不安全）。
	Enabled bool `toml:"enabled"`
	// NetworkPolicy 网络访问策略：
	//   "block"（默认）— 禁止所有出站网络，对齐 Claude Code 默认行为
	//   "allow"        — 允许所有出站网络
	NetworkPolicy string `toml:"network_policy"`
	// AllowedDomains NetworkPolicy="allow" 时的出站域名白名单（linux bwrap 暂不支持，macOS Seatbelt 支持）。
	// 空列表 = 不限制域名（仅在 allow 模式下有意义）。
	AllowedDomains []string `toml:"allowed_domains"`
	// BwrapPath Linux 下 bubblewrap 可执行文件路径。空 = 自动 PATH 查找。
	BwrapPath string `toml:"bwrap_path"`
	// AllowedPaths Agent 可访问的额外文件系统路径白名单。
	// DataDir（~/.polarisagi/polaris）始终自动包含，无需重复填写。
	// 典型用途：将用户项目目录加入白名单，让 Agent 可读写项目文件并在该目录执行命令。
	// 示例：["/home/user/projects", "/tmp/scratch"]
	// 注意：bash/run_command 工具的进程沙箱仅允许读写这些路径（OS 级强制隔离）。
	AllowedPaths []string `toml:"allowed_paths"`
}

// CompressorConfig 上下文压缩器配置，对齐 Claude Code 百分比阈值模型。
type CompressorConfig struct {
	// ContextWindow 模型上下文窗口大小（token 数）。
	// 自动压缩阈值 = ContextWindow × AutoCompactPct / 100。
	// 0 = 使用内置默认值 32768（Tier-0 保守值）。
	ContextWindow int `toml:"context_window"`
	// AutoCompactPct 自动压缩触发百分比（1~100）。
	// 对齐 Claude Code 默认值 95。0 = 使用内置默认值。
	AutoCompactPct float64 `toml:"auto_compact_pct"`
	// WarnPct 上下文使用率警告百分比，低于 AutoCompactPct 时提前告警。
	WarnPct float64 `toml:"warn_pct"`
	// MaxThrashCount 连续自动压缩但仍超阈值的最大次数，超出后停止自动压缩并告警。
	MaxThrashCount int `toml:"max_thrash_count"`
}

func loadModuleTOML(modulePath string, target interface{}) error {
	if _, err := os.Stat(modulePath); os.IsNotExist(err) {
		return nil
	}
	data, err := os.ReadFile(modulePath)
	if err != nil {
		slog.Error("polaris: failed to read threshold override", "file", modulePath, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "loadModuleTOML", err)
	}
	if err := toml.Unmarshal(data, target); err != nil {
		slog.Error("polaris: failed to parse threshold override", "file", modulePath, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "loadModuleTOML", err)
	}
	slog.Info("polaris: threshold override loaded", "file", modulePath)
	return nil
}

func Load(path string) (*Config, error) {
	// 1. 先以 defaults.toml 作为基底（保证所有字段有默认值）
	defaultsData, err := configs.FS.ReadFile("defaults.toml")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load: read embedded defaults", err)
	}
	cfg := &Config{Thresholds: DefaultThresholds()}
	if err := toml.Unmarshal(defaultsData, cfg); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load: parse embedded defaults", err)
	}

	// 2. 若用户 config.toml 存在，叠加覆盖（仅写入的字段生效，其余保留 defaults）
	userData, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := toml.Unmarshal(userData, cfg); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("Load: parse %s", path), err)
		}
	} else {
		// 用户配置不存在，导出 defaults 供后续手动编辑（幂等，失败忽略）
		if errMkdir := os.MkdirAll(filepath.Dir(path), 0755); errMkdir == nil {
			os.WriteFile(path, defaultsData, 0600) //nolint:errcheck
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Load", err)
	}
	return cfg, nil
}

// Validate 对边界非法值做 Fail-Fast 校验，防止明显错误配置在运行期才暴露 panic。
// 未填写的字段（零值）代表"使用系统默认"，不视为错误。
func (c *Config) Validate() error {
	// 若 TTS 未指定 sherpa_version，则自动复用 STT 的版本
	if c.Inference.TTS.SherpaVersion == "" {
		c.Inference.TTS.SherpaVersion = c.Inference.STT.SherpaVersion
	}

	if c.System.Tier < 0 || c.System.Tier > 3 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: system.tier must be 0-3, got %d", c.System.Tier))
	}
	// go_memlimit_mb 为 0 代表不设 GOMEMLIMIT（由运行时自动管理），合法。
	// 非零时要求最低 64MB，低于此值会导致频繁 GC 甚至 OOM。
	if c.System.GoMemLimitMB != 0 && c.System.GoMemLimitMB < 64 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("config: system.go_memlimit_mb must be >= 64 when set, got %d", c.System.GoMemLimitMB))
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
			return nil, apperr.Wrap(apperr.CodeInternal, "LoadThresholds", err)
		}
	}

	return &t, nil
}
