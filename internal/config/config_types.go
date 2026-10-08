package config

// CognitionConfig SurrealDB 认知存储后端配置（ADR-0003）。
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
	DataEncryptionKey    string                 `toml:"data_encryption_key"`
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
// 典型场景：logs_dir 指向中央日志盘；db_dir 指向高速 NVMe；workspace_dir 指向 tmpfs；models_dir 指向大容量存储。
type DirsConfig struct {
	LogsDir       string `toml:"logs_dir"`       // 覆盖 DataDir/logs（系统运行日志）
	DBDir         string `toml:"db_dir"`         // 覆盖 DataDir/data（SQLite + SurrealDB 数据库文件）
	WorkspaceDir  string `toml:"workspace_dir"`  // 覆盖 DataDir/workspace（Agent VFS 任务沙箱）
	ModelsDir     string `toml:"models_dir"`     // 覆盖 DataDir/models（本地 AI 模型权重资产）
	BinDir        string `toml:"bin_dir"`        // 覆盖 DataDir/bin（外部依赖可执行文件）
	ConfigDir     string `toml:"config_dir"`     // 覆盖 DataDir/config（配置覆盖、提示词、SOUL.md）
	ExtensionsDir string `toml:"extensions_dir"` // 覆盖 DataDir/extensions（插件市场安装目录）
	SkillsDir     string `toml:"skills_dir"`     // 覆盖 DataDir/skills（用户自定义技能脚本）
	SessionsDir   string `toml:"sessions_dir"`   // 覆盖 DataDir/sessions（会话历史转录记录）
	AuditDir      string `toml:"audit_dir"`      // 覆盖 DataDir/audit（不可变审计日志与归档）
	CacheDir      string `toml:"cache_dir"`      // 覆盖 DataDir/cache（HTTP/推理缓存）
	HooksDir      string `toml:"hooks_dir"`      // 覆盖 DataDir/hooks（用户事件触发钩子）
	TmpDir        string `toml:"tmp_dir"`        // 覆盖 DataDir/tmp（临时下载解压暂存）
	RunDir        string `toml:"run_dir"`        // 覆盖 DataDir/run（运行时状态 PID/端口/锁文件）
	SecretsDir    string `toml:"secrets_dir"`    // 覆盖 DataDir/secrets（主密钥与敏感凭据保护区）
	EvalDir       string `toml:"eval_dir"`       // 覆盖 DataDir/eval（评测与基准数据集）
}

type InferenceConfig struct {
	DefaultProvider   string      `toml:"default_provider"`
	ReasoningProvider string      `toml:"reasoning_provider"`
	StructuredOutput  string      `toml:"structured_output"`
	EmbedderDim       int         `toml:"embedder_dim"` // vector dimension; changes on local_only toggle
	Cache             CacheConfig `toml:"cache"`
	STT               STTConfig   `toml:"stt"`
	TTS               TTSConfig   `toml:"tts"`
	Audio             AudioConfig `toml:"audio"`
}

// AudioConfig 是 STT/TTS 共用的运行时生命周期配置（ADR-0107）。
type AudioConfig struct {
	// IdleUnloadMinutes 引擎最后一次使用后空闲多久卸载以释放内存；0 = 不卸载。
	// 为什么默认卸载：桌面场景与用户其他应用共享内存，STT≈420MB + MeloTTS≈530MB 不应常驻；
	// 2GB VPS 上更是核心路径之外的纯开销。下次请求自动重新加载。
	IdleUnloadMinutes int `toml:"idle_unload_minutes"`

	// AutoInstall 是否在守护进程启动后后台串行预置语音模型（STT→TTS，ADR-0108）。
	AutoInstall bool `toml:"auto_install"`
}

// EmbeddingConfig 向量化服务配置。
// BaseURL 留空 = 禁用 Tier 2（降级到词元重叠 Tier 1）。
// 兼容任何 OpenAI /v1/embeddings 兼容端点（DeepSeek-Embed / OpenAI / Jina 等）。
type EmbeddingConfig struct {
	BaseURL   string  `toml:"base_url"`             // 例: "https://api.deepseek.com/v1"
	Model     string  `toml:"model"`                // 例: "deepseek-embed"
	APIKey    string  `toml:"api_key"`              // 空 → 读 POLARIS_EMBEDDING_API_KEY 环境变量
	Threshold float64 `toml:"similarity_threshold"` // 余弦阈值，默认 0.60
	Backend   string  `toml:"backend"`              // "auto"|"onnx"|"ollama"|"llama_server"|"none"，默认 "auto" (ADR-0109)
	ONNXModel string  `toml:"onnx_model"`           // "auto"|"embeddinggemma"|"bge-small-zh"，默认 "auto" (ADR-0109)
	Dim       int     `toml:"dim"`                  // 显式指定向量维度；显式本地服务时必填 (ADR-0109)
}

// STTConfig 语音识别配置。模型与动态库的 URL/sha256 不在此配置，统一见
// internal/llm/audioassets 清单（URL 可配置则 sha256 无法钉死，校验形同虚设）。
type STTConfig struct {
	// SherpaVersion 留空取代码内 audioassets.SherpaABIVersion；填写且与之不同则 STT/TTS 资产初始化
	// 报错（FFI 结构体偏移按该版本钉死，换版本会内存破坏）。
	SherpaVersion string `toml:"sherpa_version"`
	// UseITN 是否启用 SenseVoice 逆文本规范化。默认 false：实测 use_itn=1 会丢首字、
	// 错词（"开放时间"→"放时间"、"FIFTY"→"FIFT"），官方 sherpa-onnx-offline 同模型同参数
	// 输出逐字节一致，属模型 ITN 路径缺陷。
	UseITN bool `toml:"use_itn"`
	// Language 指定识别语言："zh"（默认，中文，最准确）、"en"、"ja"、"ko"、"yue"（粤语）或 "auto"（自动检测）。
	Language string `toml:"language"`
}

// TTSConfig TTS 引擎配置。服务端 provider 两种：
//   - ""/"sherpa" 本地 sherpa-onnx MeloTTS / Matcha（离线；按硬件三档选模型，按需下载，ADR-0110）
//   - "http"      外部 HTTP sidecar（CosyVoice 2 / Qwen3-TTS 等 GPU 推理服务，高级可选）
//
// Edge TTS 已于 ADR-0107 删除；旧配置 provider="edge" 加载时迁移为 "sherpa" 并 Warn。
type TTSConfig struct {
	// Provider 指定服务端 TTS 引擎类型：""/"sherpa" | "http"。留空等价于 "sherpa"。
	Provider string `toml:"provider"`

	// Engine 前端朗读引擎偏好："auto"（默认：服务端就绪则用服务端，否则退回系统语音）|
	// "server"（只用服务端）| "system"（只用浏览器/WebView 内置系统语音，声音在用户设备上
	// 合成，服务器零开销）。后端不据此行事，只经 capabilities 下发给前端。
	Engine string `toml:"engine"`

	// ── sherpa provider 专属 ─────────────────────────────────────────────────

	// SherpaVersion 与 STT 共用同一 sherpa-onnx 版本（共享动态库）。
	// 留空时自动复用 inference.stt.sherpa_version。
	SherpaVersion string `toml:"sherpa_version"`
	// Model 服务端 TTS 模型："auto"（默认，按硬件三档：B 档 Matcha / C 档 MeloTTS）|
	// "melo" | "matcha"。显式指定可越档（受空闲内存门槛与基准门控约束），但不能解锁 A 档
	// （内存 <1800MB 或 <2 核：服务端语音整体关闭）。两个模型均为单说话人，无说话人选项
	// （原 kokoro_sid 已随 Kokoro 删除，ADR-0110）。
	Model string `toml:"model"`
	// Speed 语速倍率，默认 1.0。
	Speed float64 `toml:"speed"`

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
	Engines              map[string]string `toml:"engines"`
	Tier0VectorScanLimit int               `toml:"tier0_vector_scan_limit"`
	SQLiteDBFileName     string            `toml:"sqlite_db_filename"`  // 覆盖默认 polaris.db
	SurrealDBFileName    string            `toml:"surreal_db_filename"` // 覆盖默认 surreal.db
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

	// TrustedWorkspaceRoots 用户显式声明信任的工作区**绝对路径**前缀。
	// 命中的工作区，其 AGENTS.md/CLAUDE.md/.polaris_context.md 会作为项目级
	// 系统指令写入 ZoneImmutable；未命中（默认，空列表）则一律按 TaintHigh
	// 进 ZoneExternalCatalog 并加围栏（GD-14-005 / ADR-0088 决策三）。
	//
	// 默认必须为空：Agent 处理 clone 来的仓库时，其中的 AGENTS.md 完全是
	// 攻击者可控的。信任只能由用户针对具体路径主动授予，不得由"文件名恰好
	// 是约定名字"推定。相对路径会被忽略（无法可靠判定信任边界）。
	TrustedWorkspaceRoots []string `toml:"trusted_workspace_roots"`

	// HITLTrustMinApprovals 自适应降级阈值（GD-14-004）：同一 Agent 对同类
	// 低风险 checkpoint 连续获得多少次**人工批准**后，后续同类请求降级为
	// 通知而非阻塞审批。0（默认）= 完全关闭。
	//
	// 默认必须为 0：自适应降级本质是削弱安全边界，开启前应先用
	// polaris.hitl.decisions_total 指标确认哪些 checkpoint_type 的 human
	// 批准率确实长期接近 100%（即已退化为习惯性点击），再按类开启。
	// 即使开启，也只降到"通知"，且污点/高风险/设备操控/L4 晋升一律不参与。
	HITLTrustMinApprovals int `toml:"hitl_trust_min_approvals"`

	// HITLTrustWindowHours 信任累积的有效期（小时）。0 时取 24。
	// 信任不应无限期留存——"三个月前批过 10 次"不构成今天放行的理由。
	HITLTrustWindowHours int `toml:"hitl_trust_window_hours"`
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

	// TaskRetentionTTLSec 终态任务（done/failed）在 tasks 表中的保留秒数，
	// 到期由 Blackboard Reaper 归档进 decision_log 后物理删除（GD-13-004）。
	// 0 表示用代码默认值 24h；低于 5 分钟的取值会被 SetTaskRetentionTTL 拒绝。
	//
	// 单位后缀写进字段名/键名而非用 time.Duration：BurntSushi/toml 不能把
	// "24h" 这类字符串解码进 time.Duration，本项目全部时长配置一律采用
	// `xxx_ms` / `xxx_sec` 整数（见 internal/config/thresholds.go）。
	// 初版曾用 `TaskRetentionTTL time.Duration` + `task_retention_ttl = "24h"`，
	// 直接导致 **内嵌 defaults.toml 解析失败 → Load() 整体报错 → 二进制无法启动**。
	TaskRetentionTTLSec int `toml:"task_retention_ttl_sec"`
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
	Engine           string `toml:"engine"`
	DefaultBlock     bool   `toml:"default_block"`
	CedarEnforceMode string `toml:"cedar_enforce_mode"` // "shadow" (default), "deny", "full"
	// HardConstraintsPath 硬约束 Cedar 策略文件的磁盘路径；空 = 使用二进制内置 embed 默认策略。
	// 设置后在进程启动时从磁盘加载，支持不重新编译替换策略（运营商自定义场景）。
	HardConstraintsPath string `toml:"hard_constraints_path"`
	// SoftConstraintsPath 软约束 Cedar 策略文件的磁盘路径；空 = 使用二进制内置 embed 默认策略。
	// 软约束可热更新：调用 Gate.ReloadCedarPoliciesFromDisk 无需重启进程。
	SoftConstraintsPath string `toml:"soft_constraints_path"`
}

type EvalConfig struct {
	CIGate         bool     `toml:"ci_gate"`
	ShadowDeploy   bool     `toml:"shadow_deploy"`
	SafetyKeywords []string `toml:"safety_keywords"`
}

type InterfaceConfig struct {
	Host      string `toml:"host"`
	Port      int    `toml:"port"`
	CLI       bool   `toml:"cli"`
	HTTP      bool   `toml:"http"`
	GRPC      bool   `toml:"grpc"`
	WebSocket bool   `toml:"websocket"`
	// AppsSandboxPort MCP Apps（io.modelcontextprotocol/ui）沙箱代理专用监听端口
	// （M8f-1）。规范要求宿主页面与 Sandbox proxy 必须异源（apps_spec.mdx
	// "Sandbox proxy" 第 1 条），故网关另开一个监听器只托管沙箱代理页。
	// 0 = Port+1；Port 也为 0 时由操作系统分配（见 server.NewMCPAppsSandboxConfig）。
	AppsSandboxPort int `toml:"apps_sandbox_port"`
	// AppsSandboxOrigin 反向代理部署时对外可见的沙箱源（如 TLS 终结在代理层，
	// 内部端口不等于外部可见地址）；空 = 前端按自身 hostname + 沙箱端口推导。
	AppsSandboxOrigin string `toml:"apps_sandbox_origin"`
	// AppsHostOrigin 反向代理部署时宿主页面（Web UI）对外可见的源，加入沙箱页
	// frame-ancestors；直连部署留空（按浏览器访问的 hostname 自动放行）。
	AppsHostOrigin string `toml:"apps_host_origin"`
	// AppsEnabled 控制 MCP Apps 能力总开关（含沙箱监听器是否启动、
	// GET /v1/mcp-apps/config 的 enabled 字段）。默认 true。
	AppsEnabled bool `toml:"apps_enabled"`
}

type SecurityConfig struct {
	LocalOnlyMode bool `toml:"local_only_mode"`
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
	// BwrapPath Linux 下 bubblewrap 可执行文件路径。空 = 自动 PATH 查找。
	BwrapPath string `toml:"bwrap_path"`
	// AllowedPaths Agent 可访问的额外文件系统路径白名单。
	// DataDir（~/.polaris）始终自动包含，无需重复填写。
	// 典型用途：将用户项目目录加入白名单，让 Agent 可读写项目文件并在该目录执行命令。
	// 示例：["/home/user/projects", "/tmp/scratch"]
	// 注意：bash/run_command 工具的进程沙箱仅允许读写这些路径（OS 级强制隔离）。
	AllowedPaths []string `toml:"allowed_paths"`
	// Remote 远端云沙箱（Sbx-L4）配置，见 RemoteSandboxConfig。
	Remote RemoteSandboxConfig `toml:"remote"`
	// AllowTrustedInProcessFallback 可信来源（TrustOfficial 及以上）在
	// Wasm/Container/Remote 均不可用时是否允许降级到 InProcess 执行。
	// 默认 false（fail-closed）：可信 ≠ 稳定，可信来源的代码仍可能有死循环/
	// 内存爆炸/panic，InProcess 执行会直接拖垮宿主进程——这是稳定性维度的
	// 风险，与"可信"这一安全维度的判断是两回事，不应被静默混为一谈
	// （阶段03 R-05）。仅开发环境或明确接受该风险时置 true。
	AllowTrustedInProcessFallback bool `toml:"allow_trusted_inprocess_fallback"`
}

// RemoteSandboxConfig 远端云沙箱（Sbx-L4）配置。可选能力，非硬依赖（[Tier-0-Limit]）。
// 用途：Tier-0 本地无法启动 L3 容器沙箱（内存不足）或需要更高计算力/隔离度时
// （如 Python 数据科学类 Skill），将工具执行委托给远端 HTTP 执行器（自托管 VPS、
// E2B、Modal、Daytona 等任意兼容端点）。涉及代码/数据离开本机发往第三方服务，
// 默认关闭，需运营者显式配置 Endpoint 后启用。
// 架构文档: docs/arch/00-Global-Dictionary.md §0 Sbx-L4；docs/arch/M07-Tool-Action-Layer.md §4.4。
type RemoteSandboxConfig struct {
	// Enabled 是否启用远端沙箱委托。默认 false。
	Enabled bool `toml:"enabled"`
	// Endpoint 远端执行器根 URL，如 "https://executor.example.com"。Enabled=true 时必填，
	// 为空则跳过初始化并记录 Warn（fail-closed，不阻塞启动）。
	Endpoint string `toml:"endpoint"`
	// AuthToken Bearer 认证令牌。空 → 读 POLARIS_REMOTE_SANDBOX_TOKEN 环境变量
	// （避免密钥明文写入配置文件，对齐 EmbeddingConfig.APIKey 先例）。
	AuthToken string `toml:"auth_token"`
	// TimeoutSec 单次调用超时秒数。0 = 默认 300s（对应重计算场景）。
	TimeoutSec int `toml:"timeout_sec"`
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

// A2AConfig 控制 Agent-to-Agent (A2A) 接口配置。
type A2AConfig struct {
	Enabled bool     `toml:"enabled"`
	Name    string   `toml:"name"`
	Skills  []string `toml:"skills"`
}
