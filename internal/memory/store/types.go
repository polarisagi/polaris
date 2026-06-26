package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

type ImmutableCore struct {
	AgentName            string            `json:"agent_name"`
	AgentRole            string            `json:"agent_role"`
	ModelID              string            `json:"model_id"`
	BuiltinTools         string            `json:"builtin_tools"`
	InstalledPlugins     string            `json:"installed_plugins"`
	UserPreferences      map[string]string `json:"user_preferences"`
	GlobalGoal           string            `json:"global_goal"`
	SystemPromptTemplate string            `json:"system_prompt_template"`

	// 三层系统提示词组装字段（stable + volatile）

	// SoulMDContent 用户自定义身份文件内容（~/.polarisagi/polaris/config/SOUL.md）。
	// 非空时替换 DefaultPolarisIdentity 作为 stable 层首段。
	SoulMDContent string `json:"soul_md_content,omitempty"`

	// ModelGuidance 模型专属工具调用引导，由 M13 Interface 层按 ModelID 注入到 stable 层。
	ModelGuidance string `json:"model_guidance,omitempty"`

	// PlatformHint 平台感知提示词，由 M13 Interface 层按接入平台注入到 stable 层末尾。
	// 取值来自 memory.PlatformHints 映射（cli/webui/api/cron）。
	PlatformHint string `json:"platform_hint,omitempty"`

	// VolatileBlock 易变信息区（时间戳/会话 ID/模型信息），每轮刷新。
	// 精确到天而非分钟，确保同一天内 prefix cache 不失效。
	VolatileBlock string `json:"volatile_block,omitempty"`

	// AmbientContext ambient skill 上下文（instructions + 目录索引行）。
	// 由 buildAmbientSkillsSection 填充，在 renderSystemPrompt 完成后追加到尾部。
	// 不进入 Go template 解析流程，避免 skill instructions 中的 {{ }} 破坏模板解析。
	AmbientContext string `json:"ambient_context,omitempty"`

	// CustomInstructions 用户追加的行为指令（stable 层末尾，追加而非覆盖身份）。
	// 来源：~/.polarisagi/polaris/config/prompts/custom_instructions.md 或 Web UI 编辑。
	// DB 删除不影响（文件持久化），factory reset 时才清空。
	CustomInstructions string `json:"custom_instructions,omitempty"`

	// OperationalDirectives 高级操作指令集合（含 Tool-Use, Task Completion 等）。
	OperationalDirectives string `json:"operational_directives,omitempty"`
}

func NewImmutableCore() *ImmutableCore {
	return &ImmutableCore{
		AgentName:       "Polaris (北极星)", // default name
		AgentRole:       "一个开源自托管 AI Agent",
		UserPreferences: make(map[string]string),
	}
}

type ActiveContext struct {
	CurrentTask        *Task
	RecentObservations []Observation
	RetrievedContext   []MemoryFragment
	TaintLevel         int
}

// Rebuild 重建 ActiveContext 状态。
// 在方法内回放最近的 event 来重构状态。如重建耗时 > 500ms，需通过 slog.Warn 发出警告。
func (ac *ActiveContext) Rebuild(ctx context.Context, events []types.Event) error {
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		if duration > 500*time.Millisecond {
			slog.Warn("cognition: ActiveContext.Rebuild exceeded 500ms SLA", "duration_ms", duration.Milliseconds(), "events", len(events))
		}
	}()

	// 重建状态：最多处理最近 1000 条
	limit := len(events)
	if limit > 1000 {
		events = events[limit-1000:]
	}

	for _, e := range events {
		// MVP 占位：实际应根据 e.Type 更新 ac.CurrentTask / ac.RecentObservations
		_ = e
	}
	return nil
}

// Task 当前任务。
type Task struct {
	ID          string
	Description string
	Goal        string
	InputTypes  []string
	OutputTypes []string
	DomainHint  string
}

// Observation 环境观察。
type Observation struct {
	Step      int
	Content   string
	ToolName  string
	ToolInput []byte
	Timestamp int64
}

// MemoryFragment 检索到的记忆片段。
type MemoryFragment struct {
	ID       string
	Content  string
	Source   string // "episodic" | "semantic" | "procedural"
	Score    float64
	Metadata map[string]string
}

type UserProfile struct {
	ID                 string
	Namespace          string
	ExplicitPrefs      map[string]string
	SafetyRules        map[string]string
	ImplicitPrefs      *ImplicitPreferences
	InteractionSummary string
	Version            int64
}

// ImplicitPreferences 隐式偏好。
type ImplicitPreferences struct {
	CodingStyle         string
	ToolUsage           map[string]float64
	ModelTierPref       string
	InteractionPatterns []string
	DomainKnowledge     map[string]float64
}

const (
	TaintNone     = 0
	TaintLow      = 1
	TaintMedium   = 2
	TaintHigh     = 3
	TaintCritical = 4
)
