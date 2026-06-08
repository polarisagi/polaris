package protocol

import "time"

// EventType enumerates structured coordination event kinds for the blackboard.
type EventType string

const (
	EventIntent        EventType = "intent"
	EventClaim         EventType = "claim"
	EventResult        EventType = "result"
	EventFail          EventType = "fail"
	EventCancel        EventType = "cancel"
	EventHeartbeat     EventType = "heartbeat"
	EventActionPending EventType = "action_pending"
	EventActionDone    EventType = "action_done"

	// M9 自改善闭环事件 — 跨模块走结构化事件（XR-04，禁字符串隐式耦合）
	// 发布方：pkg/swarm.ReflexionEngine；订阅方：pkg/swarm/self_improve.Engine 内环
	EventHeuristicGenerated EventType = "heuristic_generated"
	// 发布方：pkg/governance/eval.RunnerImpl；订阅方：pkg/swarm/self_improve.Engine 外环
	EventEvalCompleted EventType = "eval_completed"
)

// EventStatus tracks lifecycle.
type EventStatus string

const (
	StatusPending   EventStatus = "pending"
	StatusClaimed   EventStatus = "claimed"
	StatusExecuting EventStatus = "executing"
	StatusDone      EventStatus = "done"
	StatusFailed    EventStatus = "failed"
)

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
type OutboxEvent struct {
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

type OutboxStatus string

const (
	OutboxPending OutboxStatus = "pending"
	OutboxClaimed OutboxStatus = "claimed"
	OutboxDone    OutboxStatus = "done"
	OutboxDead    OutboxStatus = "dead" // exceeded MaxRetries
)

// Event is the unit of structured coordination on the blackboard.
// Natural language content goes in Payload; coordination metadata is typed.
type Event struct {
	ID        string        `json:"id"`
	Type      EventType     `json:"type"`
	Status    EventStatus   `json:"status"`
	TaskID    string        `json:"task_id"`
	AgentID   string        `json:"agent_id,omitempty"`
	Payload   []byte        `json:"payload,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	TTL       time.Duration `json:"ttl,omitempty"`
}

// HeuristicGeneratedPayload Reflexion 生成启发式规则后的事件 payload。
// 对应 EventType = EventHeuristicGenerated。
// 发布方在步骤3（GeneratedHeuristic 写入后）发布；订阅方更新 ErrorPatternMemory。
type HeuristicGeneratedPayload struct {
	TaskID    string `json:"task_id"`
	TaskType  string `json:"task_type"`
	Heuristic string `json:"heuristic"`  // GeneratedHeuristic 内容
	AvoidRule string `json:"avoid_rule"` // 从 Cause 提取的规避规则
	CreatedAt int64  `json:"created_at"`
}

// EvalCompletedPayload Eval Suite 运行完成后的事件 payload。
// 对应 EventType = EventEvalCompleted。
// 发布方在 RunSuite 返回后发布；订阅方更新 prompt_versions.score 并决定是否触发 Rollout。
type EvalCompletedPayload struct {
	Suite       string  `json:"suite"`        // "training" | "validation"
	CandidateID string  `json:"candidate_id"` // prompt_versions.id，空表示基线评测
	PassRate    float64 `json:"pass_rate"`    // 0.0~1.0
	BlockDeploy bool    `json:"block_deploy"` // safety_fail>0 时为 true
	RunID       string  `json:"run_id"`
	CreatedAt   int64   `json:"created_at"`
}
