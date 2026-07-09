package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ImmutableCore 永不裁剪的核心区（M05 §2.2），写入经 M9 staging + M11 闸控。
// 可写字段集合内嵌 protocol.ImmutableCoreFields（M04 §B2 跨模块共享类型），
// 外部消费方（gateway）经 protocol.ImmutableCore.Fields() 读写，不再需要
// 类型断言到本具体类型。字段级注释见 protocol/immutable_core.go。
type ImmutableCore struct {
	protocol.ImmutableCoreFields
}

// Fields 返回可写字段集合指针，实现 protocol.ImmutableCore.Fields()。
func (ic *ImmutableCore) Fields() *protocol.ImmutableCoreFields {
	return &ic.ImmutableCoreFields
}

func NewImmutableCore() *ImmutableCore {
	return &ImmutableCore{
		ImmutableCoreFields: protocol.ImmutableCoreFields{
			AgentName:       "Polaris (北极星)", // default name
			AgentRole:       "一个开源自托管 AI Agent",
			UserPreferences: make(map[string]string),
		},
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
