package agent

// 2026-08-02 从 agent_execute_util.go 拆分（Test_inv_FileLineLimit R7 400 行上限
// 存量债务，见 local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现）：
// 本文件收敛 Episodic 记忆写入与其失败熔断处理，纯搬运无行为变更。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func (a *Agent) injectMemoryToMsgs(ctx context.Context, msgs []types.Message) []types.Message {
	if a.assembler == nil || a.sCtx.TaskModel == nil {
		return msgs
	}

	maxT := a.sCtx.RawIntentTS.Source.OriginTaintLevel
	if maxT == types.TaintNone {
		maxT = types.TaintHigh
	}
	req := agentctx.AssembleRequest{
		Query:                 a.sCtx.TaskModel.Goal,
		SessionKey:            a.sCtx.SessionID,
		MaxTokens:             2000,
		MaxTaint:              maxT,
		IncludeKnowledge:      true,
		SurpriseHint:          metrics.GlobalSurpriseIndex().Current(),
		SurpriseHintThreshold: a.Config.SurpriseHintThreshold,
	}

	ac, err := a.assembler.Assemble(ctx, req)
	if err != nil || len(ac.Items) == 0 {
		return msgs
	}

	var sb strings.Builder
	sb.WriteString("Relevant Context:\n")
	for _, item := range ac.Items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.Source, item.Content)
	}

	return append([]types.Message{{Role: "system", Content: sb.String()}}, msgs...)
}

func (a *Agent) writeEpisodicWithExtract(ctx context.Context, ev types.Event) {
	if a.memory == nil {
		return
	}
	if err := a.memory.AppendEpisodicEvent(ctx, ev, types.TaintNone); err != nil {
		a.handleMemoryPersistenceFailure(ctx, err, ev)
		return
	}

	if a.outboxWriter == nil {
		return
	}

	switch string(ev.Type) {
	case "task_perceived", "plan_generated", "reflection_completed", "execution_completed", string(types.EventActionPending), string(types.EventActionDone):
		sessionID := ev.TaskID
		if sessionID == "" && a.sCtx != nil {
			sessionID = a.sCtx.SessionID
		}
		// 幂等键不能只用 ev.ID（2026-07-22 一致性审查修复）：ev.ID 由
		// fsm.StateMachine.NextEventID 生成，形如 "{sessionID}:{seq}:{eventType}"，
		// 其中 seq（sm.eventSeq）是*每个 Agent 实例*的私有计数器——按设计
		// （见 NextEventID 文档："同 session+seq → 同 ID，不依赖 wall clock"，
		// 满足 inv_M4_02 崩溃恢复重放确定性要求）在新建 Agent 实例时重置为 0。
		// Pool.Acquire 对同一 sessionID 的每一轮新对话都会在上一轮终态后构造
		// 全新 Agent 实例，因此不同轮次的 seq 序列会重复，ev.ID 本身也会重复
		// ——若直接拿 ev.ID 做 outbox 幂等键后缀，第二轮起会撞
		// idempotency_key UNIQUE 约束，导致 TopicEpisodicExtract 语义抽取
		// 触发从第二轮起被静默丢弃。这里刻意不改 NextEventID/ev.ID 本身
		// （那是重放确定性的必要不变量），只在 outbox 键这一层追加
		// outboxUniqueSuffix()：本调用同步执行一次、无重试语义，足以解决。
		// GR-4-001 补漏：本站点与 agent_execute_effect_helpers.go 的 5 处同源，
		// 但当轮修复只覆盖了后者。
		a.emitOutbox(ctx, protocol.TopicEpisodicExtract, "episodic_extract", map[string]any{
			"session_id": sessionID,
			"event_type": string(ev.Type),
			"content":    string(ev.Payload),
		}, ev.ID+":extract:"+outboxUniqueSuffix(), "episodic_extract")
	}
}

// emitOutbox 构造并投递一条 outbox 事件，构造/写入失败均记录 Error 级告警。
//
// 存在意义（HE-6 + HE-3）：Agent 的 6 个 outbox 投递点此前都是
// `ev, _ := protocol.NewOutboxEvent(...)` + `Write(ctx, ev)` 的复制粘贴。
// 两个错误都必须处理，且处理方式完全一致：
//   - NewOutboxEvent 失败返回**零值** OutboxEntry（TargetEngine/Payload/幂等键
//     全空）。若不拦截就 Write，落库的是一条没有目标引擎、无法被任何
//     OutboxWorker 路由的垃圾记录，且会永久占用一个空幂等键槽位；
//   - Write 失败意味着该派生事件永久丢失（无重试语义），必须告警。
//
// 这些投递都是主控制流之外的 side-effect（记忆投影/语义抽取/巩固触发），
// 失败不得中断 Agent 执行——因此只告警不返回错误，也不改变调用点控制流。
func (a *Agent) emitOutbox(ctx context.Context, topic, op string, payload any, idemKey, what string) {
	if a.outboxWriter == nil {
		return
	}
	ev, err := protocol.NewOutboxEvent(topic, op, payload, idemKey)
	if err != nil {
		slog.ErrorContext(ctx, "agent: build outbox event failed, derived event dropped",
			"agent_id", a.ID, "topic", topic, "what", what, "err", err)
		return
	}
	if err := a.outboxWriter.Write(ctx, ev); err != nil {
		slog.ErrorContext(ctx, "agent: outbox write failed, derived event may be lost",
			"agent_id", a.ID, "topic", topic, "what", what, "err", err)
	}
}

// handleMemoryPersistenceFailure 处理 Episodic 写入失败（GD-13-003 FSM 熔断）。
//
// HE-6（State-in-DB）：Episodic 事件是 Agent 状态外化的核心路径之一，Reflect/Replan
// 等后续决策均依赖历史事件序列。持久化失败若被静默吞掉（旧实现 `_ = a.memory.
// AppendEpisodicEvent(...)`），会导致 Agent 在残缺状态上继续决策而不自知——违反
// HE-1（可观测优先）与 HE-6。
//
// 只对存储层不可用（CodeStorageUnavailable）熔断；序列化失败等其他 CodeInternal
// 错误只记录日志不熔断，避免偶发的单条事件构造失败误杀整条执行链路。
//
// 熔断机制复用 agent_execute_dag.go capability_gap 先例，不新增 FSM 转换规则、
// 不新增挂起态类型（HE-3 可组合原语 + ADR-0042 先例）：
//  1. 设置 sCtx.SuspendReason，供 HITL/运维侧观测挂起原因；
//  2. 经 outbox 异步投递 m9_storage_degraded 事件，供运维告警/自动恢复 Worker 消费；
//  3. 调用 a.asyncIntent(TriggerInterruptReceived) —— 该 trigger 在
//     fsm/state_machine.go Dispatch() 中作为状态无关的全局处理（见该文件 S_INTERRUPT
//     通用处理分支），可从任意非终态直接进入 S_INTERRUPT，无需在 transitions.go
//     注册表中新增专用转换规则。
func (a *Agent) handleMemoryPersistenceFailure(ctx context.Context, err error, ev types.Event) {
	if !isMemoryPersistenceFailure(err) {
		slog.Error("writeEpisodicWithExtract: episodic 写入失败（非存储层故障，不熔断）",
			"error", err, "event_type", ev.Type, "task_id", ev.TaskID)
		return
	}

	slog.Error("writeEpisodicWithExtract: 检测到存储层持久化失败，触发 FSM 熔断",
		"error", err, "event_type", ev.Type, "task_id", ev.TaskID)

	if a.sCtx != nil {
		a.sCtx.SuspendReason = "memory_persistence_failure"
	}

	if sqlRepo, ok := a.taskRepo.(protocol.SQLQuerier); ok && sqlRepo != nil {
		payloadBytes, _ := json.Marshal(map[string]string{
			"error":      err.Error(),
			"event_type": string(ev.Type),
			"task_id":    ev.TaskID,
		})
		if _, execErr := sqlRepo.ExecContext(ctx, `
			INSERT INTO outbox (created_at, target_engine, operation, scope, payload, idempotency_key, status)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, time.Now().UnixMilli(), "m9_storage_degraded", "upsert", "memory_persistence_failure",
			payloadBytes, uuid.New().String(), "pending"); execErr != nil {
			slog.Error("agent: memory persistence failure outbox write failed",
				"agent_id", a.ID, "err", execErr)
		}
	}

	a.asyncIntent(types.TriggerInterruptReceived)
}

// isMemoryPersistenceFailure 判断错误是否为存储层不可用（而非序列化等其他内部错误）。
func isMemoryPersistenceFailure(err error) bool {
	return apperr.IsCode(err, apperr.CodeStorageUnavailable)
}
