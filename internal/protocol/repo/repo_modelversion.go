package repo

import "context"

// ModelVersionEntry 与 internal/protocol/schema/033_model_version_registry.sql
// model_version_entries 表字段一一对应。承载 P3-2 ModelVersionRegistry 的
// 运行时状态：版本/废弃标记/继任模型/兼容性评分/连续调用失败计数。
// @arch: docs/arch/M01-Inference-Runtime.md §9
type ModelVersionEntry struct {
	ID                 string // '{provider}:{model_id}' 复合键
	Provider           string
	ModelID            string
	Version            string
	Deprecated         bool
	SuccessorModelID   string
	PromptTemplate     string
	ToolCallStyle      string
	MaxContext         int
	Capabilities       string // JSON 对象，如 {"vision":true,"embedding":false,"tool_call":true}
	ValidatedOn        string // JSON 字符串数组，技能兼容测试通过的 skill name 列表
	CompatibilityScore float64
	ConsecutiveErrors  int
	UpdatedAt          int64
}

// ModelVersionRepository 持久化 ModelVersionEntry 的读写契约。
// @consumer: internal/llm/modelregistry.Registry
// @producer: internal/store/repo.SQLiteModelVersionRepository
type ModelVersionRepository interface {
	Get(ctx context.Context, id string) (*ModelVersionEntry, error)
	List(ctx context.Context) ([]*ModelVersionEntry, error)
	ListDeprecated(ctx context.Context) ([]*ModelVersionEntry, error)
	// FindPredecessor 查找"谁把 successor_model_id 指向了 modelID"的条目
	// （同一 provider 下），用于 RecordCallResult 连续失败自动回退时定位旧模型。
	FindPredecessor(ctx context.Context, provider, modelID string) (*ModelVersionEntry, error)
	Upsert(ctx context.Context, e *ModelVersionEntry) error
	Delete(ctx context.Context, id string) error
}
