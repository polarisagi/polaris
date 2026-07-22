package types

type

// SkillBenchmarks 技能评测基准数据。
SkillBenchmarks struct {
	PassRate     float64
	AvgLatencyMs float64
	AvgTokens    float64
}

type

// SkillFilter 技能列表过滤参数。
SkillFilter struct {
	Capabilities      []string
	RiskLevelMax      string
	IncludeDeprecated bool
}
type SkillMeta struct {
	Name            string
	Version         string // semver
	Runtime         string // script (default) / builtin
	RiskLevel       string // low / medium / high
	Sandbox         int    // Sbx-L1=1 / Sbx-L3=3
	Capabilities    []string
	ExecMode        string    // tool / ambient
	AmbientPriority string    // always / auto / index_only
	Trust           TrustTier // 替代 SignatureValid bool（ADR-0016 §2.1）
	Idempotent      bool
	Benchmarks      SkillBenchmarks
	Instructions    string // SKILL.md 全文，供 LLM tool_use 返回
	Deprecated      bool
	ScriptPath      string // marketplace 安装路径（extension_instances.install_path + "/src/index.ts"）
	// DependsOn 此技能执行前必须可用的其他技能名列表（skill:{slug} 格式）。
	// Register 时会对 DependsOn ∪ ComposesOf 做 DFS 环检测，发现环返回错误。
	DependsOn []string
	// ComposesOf 此技能聚合包含的子技能列表（超集关系）。
	ComposesOf []string
	// PluginID 是来源插件的 plugins.id（"pl_xxx"）；独立安装的技能为空。
	PluginID string
	// NeedsCompatCheck indicates reverse dependencies need compatibility testing
	NeedsCompatCheck bool
}
