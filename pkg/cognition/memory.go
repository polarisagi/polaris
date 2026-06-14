package cognition

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// 四层记忆类型定义。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §1-5

// ImmutableCore 不可变核心区（永不裁剪）。
type ImmutableCore struct {
	UserPreferences    map[string]string
	GlobalGoal         string
	SafetyConstraints  []string // M11 注入，用户不可移除
	AgentIdentity      string
	InteractionSummary string // M9 PersonaRefiner 生成，~200 tokens
}

// ActiveContext 当前上下文窗口。
type ActiveContext struct {
	CurrentTask        *Task
	RecentObservations []Observation
	RetrievedContext   []MemoryFragment
	TaintLevel         int
}

// Rebuild 重建 ActiveContext 状态。
// 在方法内回放最近的 event 来重构状态。如重建耗时 > 500ms，需通过 slog.Warn 发出警告。
func (ac *ActiveContext) Rebuild(ctx context.Context, events []protocol.Event) error {
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

// NotesStore 跨会话轻量 KV。
type NotesStore struct {
	items    map[string]*Note
	maxSize  int // 64KB 单条上限
	maxTotal int // 256KB 总容量
}

// NewNotesStore 创建跨会话轻量 KV 存储。
func NewNotesStore() *NotesStore {
	return &NotesStore{
		items:    make(map[string]*Note),
		maxSize:  65536,
		maxTotal: 262144,
	}
}

// Put 添加或更新笔记条目。
func (ns *NotesStore) Put(note *Note) bool {
	if len(note.Value) > ns.maxSize {
		return false
	}
	currentTotal := 0
	for _, n := range ns.items {
		currentTotal += len(n.Value)
	}
	if currentTotal+len(note.Value) > ns.maxTotal {
		return false
	}
	ns.items[note.Key] = note
	return true
}

// Get 获取笔记条目。
func (ns *NotesStore) Get(key string) *Note {
	return ns.items[key]
}

// Note 笔记条目。
type Note struct {
	Key       string
	Value     string
	Version   int64
	CreatedAt int64
	UpdatedAt int64
	ExpiresAt int64
	SessionID string
}

// UserProfile 用户画像（M5 §2.3）。
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
