package types

import "time"

type

// BlackboardEvent 黑板事件通知（Subscribe 订阅时返回）。
BlackboardEvent struct {
	Type      string
	TaskID    string
	AgentID   string
	Payload   []byte
	Timestamp int64
}

type

// ChangeEvent 文档变更事件（Watch 模式下返回）。
ChangeEvent struct {
	Type    string // created | updated | deleted
	Ref     *DocumentRef
	OldHash string
}

type

// TaskEvent 任务调度事件通知（Subscribe 订阅时返回）。
TaskEvent struct {
	TaskID string
	State  string // submitted / started / progress / completed / failed / cancelled
	Detail map[string]any
}

type

// IdempotencyKey 跨模块幂等键统一类型。
// 格式: {target_engine}:{entity_type}:{entity_id}:{operation}:{version}
// target_engine: "sqlite" / "surreal"
// entity_type:   "event" / "task" / "outbox" / "skill"
// entity_id:     实体唯一标识
// operation:     "create" / "update" / "delete" / "rollout"
// version:       数据版本号（int）
//
// 各模块使用规范:
//   - LLMFillEffect.IdempotencyKey — LLM 填槽结果的幂等标识（重放时跳过已执行的填槽）
//   - ToolCallRequest.IdempotencyKey — 工具调用的幂等标识（崩溃恢复时防止双重执行）
//   - Task.IdempotencyKey — 任务的幂等标识（M13 调度器去重）
//   - OutboxRecord.IdempotencyKey — Outbox 跨引擎投影的幂等标识
//   - Event.IdempotencyKey — EventLog 事件的幂等标识（M2 写入去重）
IdempotencyKey string

// BuildIdempotencyKey 按规范格式构建幂等键。
func BuildIdempotencyKey(engine, entityType, entityID, operation string, version int) IdempotencyKey {
	return IdempotencyKey(engine + ":" + entityType + ":" + entityID + ":" + operation + ":" + itoa(version))
}

type

// OutboxEvent is a row in the outbox table — the single source of truth for
// all async projections (graph build, vector index, skill deploy, event dispatch).
//
// The id column MUST be an AUTOINCREMENT integer to guarantee monotonic physical
// write order. UUIDv7 is broken for cursor polling because its random suffix
// causes lexicographic inversion under same-millisecond concurrent inserts.
//
// Worker polling uses: SELECT * FROM outbox WHERE id > :cursor AND committed_at > :last_scan
// The committed_at guard handles the case where an uncommitted row causes
// AUTOINCREMENT to skip a value before commit.
OutboxEvent struct {
	ID          int64        `json:"id"`         // AUTOINCREMENT, monotonic
	EventID     string       `json:"event_id"`   // logical Event.ID
	EventType   string       `json:"event_type"` // "graph_build" | "vector_index" | "skill_deploy"
	Payload     []byte       `json:"payload"`
	CommittedAt int64        `json:"committed_at"` // unix nano, set on INSERT
	ClaimedBy   string       `json:"claimed_by,omitempty"`
	RetryCount  int          `json:"retry_count"`
	MaxRetries  int          `json:"max_retries"`
	Status      OutboxStatus `json:"status"`
}

type

// Event is the unit of structured coordination on the blackboard.
// Natural language content goes in Payload; coordination metadata is typed.
Event struct {
	ID                string        `json:"id"`
	Type              EventType     `json:"type"`
	Status            EventStatus   `json:"status"`
	TaskID            string        `json:"task_id"`
	AgentID           string        `json:"agent_id,omitempty"`
	Payload           []byte        `json:"payload,omitempty"`
	ReasoningState    []byte        `json:"reasoning_state,omitempty"`
	EmbedModelVersion string        `json:"embed_model_version,omitempty"`
	TaintLevel        TaintLevel    `json:"taint_level,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	TTL               time.Duration `json:"ttl,omitempty"`
}

type

// HeuristicGeneratedPayload Reflexion 生成启发式规则后的事件 payload。
// 对应 EventType = EventHeuristicGenerated。
// 发布方在步骤3（GeneratedHeuristic 写入后）发布；订阅方更新 ErrorPatternMemory。
HeuristicGeneratedPayload struct {
	Seq       int64  `json:"seq"`
	TaskID    string `json:"task_id"`
	TaskType  string `json:"task_type"`
	Heuristic string `json:"heuristic"`  // GeneratedHeuristic 内容
	AvoidRule string `json:"avoid_rule"` // 从 Cause 提取的规避规则
	CreatedAt int64  `json:"created_at"`
}

type

// EvalCompletedPayload Eval Suite 运行完成后的事件 payload。
// 对应 EventType = EventEvalCompleted。
// 发布方在 RunSuite 返回后发布；订阅方更新 prompt_versions.score 并决定是否触发 Rollout。
EvalCompletedPayload struct {
	Seq              int64   `json:"seq"`
	Suite            string  `json:"suite"`        // "training" | "validation"
	CandidateID      string  `json:"candidate_id"` // prompt_versions.id，空表示基线评测
	PassRate         float64 `json:"pass_rate"`    // 0.0~1.0
	P0PassRate       float64 `json:"p0_pass_rate"` // P0用例通过率
	BlockDeploy      bool    `json:"block_deploy"` // safety_fail>0 时为 true
	WarnDeploy       bool    `json:"warn_deploy"`  // P1用例有失败时为 true（不阻断但需关注）
	SafetyViolations int     `json:"safety_violations"`
	P95LatencyMs     float64 `json:"p95_latency_ms"`
	BaselineP95Ms    float64 `json:"baseline_p95_ms"`
	RunID            string  `json:"run_id"`
	CreatedAt        int64   `json:"created_at"`
}
