package skill

// ============================================================================
// 技能库 legacy 类型定义（R7 拆分自 skill.go）。
// 架构文档: docs/arch/06-Skill-Library-深度选型.md §1
// 现行存储/传递统一使用 types.SkillMeta（见 skill.go 顶部注释与 ADR-0002）；
// 本文件类型为历史遗留，保留供参考/兼容，不参与当前 RegistryImpl 存储路径。
// ============================================================================

// Skill 是可命名、可参数化、可索引的复用技能。
type Skill struct {
	ID      string
	Name    string
	Version int

	Description  string
	Instructions string

	InputSchema   *JSONSchema
	OutputSchema  *JSONSchema
	Precondition  *Condition
	Postcondition *Condition

	WasmBytes []byte
	WasmHash  string

	Embedding []float32
	Signature string
	Tags      []string

	SuccessRate  float64
	AvgLatencyUs int64
	UseCount     int64
	LastUsedAt   int64

	RiskLevel   int
	SandboxTier int
	Source      string // builtin | llm_generated | user_defined
	SourceTrace string

	Deprecated       bool
	DeprecationLevel int
	NeedsRevalidate  bool

	DependsOn  []string
	ComposesOf []string
}

// JSONSchema 是 JSON Schema 定义。
type JSONSchema struct {
	Type       string
	Properties map[string]*JSONSchema
	Required   []string
}

// Condition 前置/后置条件。
type Condition struct {
	Description string
	Schema      *JSONSchema
}

// SkillSource 技能来源。
type SkillSource string

const (
	SkillBuiltin      SkillSource = "builtin"
	SkillLLMGenerated SkillSource = "llm_generated"
	SkillUserDefined  SkillSource = "user_defined"
)
