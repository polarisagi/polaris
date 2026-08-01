package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 阶段04 A-02（GD-13-009）：DAGModel 快照持久化，实现委派挂起崩溃无损续跑。
//
// 背景：ResumeAwaitingHandoff 此前只回填 HandoffTaskID + ForceState，
// a.sCtx.DAGModel 等执行期上下文在进程重启后彻底丢失——runExecuteDAG 命中
// nil-DAGModel 快速路径直接推进 ExecuteDone，委派节点下游的 DAG 节点全部
// 被截断，永远不会执行。本文件补齐"挂起时序列化快照 → 落盘 → 恢复时反
// 序列化 + 强制重校验 → 回填执行期上下文 → DAG 续跑跳过已完成节点"的完整
// 闭环。

// HandoffResumeContextVersion 恢复上下文序列化版本。
// 结构变更时递增；反序列化遇到未知版本一律拒绝恢复并降级为"消除死锁"旧行为，
// 禁止按当前结构强行解析（字段错位会产出"看似合法"的错误 DAG）。
const HandoffResumeContextVersion = 1

// handoffSnapshotMaxBytes 快照序列化体积上限。超过此值不落盘，只记 Warn +
// counter，避免超大 DAG 撑爆 SQLite 行。
// TODO(阶段06): 登记到 state.yaml 作为可配置阈值，当前为硬编码常量。
const handoffSnapshotMaxBytes = 256 * 1024

// HandoffResumeContext S_AWAIT_AGENT 挂起时的执行期上下文快照。
// 只持久化"重建执行所必需"的最小集合，不持久化 LLM 原始响应、提示词缓存
// 等可重算或含敏感内容的字段（PII 最小化 + 体积控制）。字段取舍原则：只收纳
// runExecuteDAG 重建执行所必需的字段；sCtx 中其余字段（RawIntentTS、
// SysEnvSnapshot、EpochTracker、LastReasoningContent 等）不入快照——它们要么
// 可从 DB/配置重算，要么含敏感内容。
type HandoffResumeContext struct {
	SchemaVersion    int              `json:"v"`
	DAGModel         *fsm.DAGModel    `json:"dag_model"`
	ExecuteResult    []byte           `json:"execute_result,omitempty"`
	CompletedNodeIDs []string         `json:"completed_node_ids,omitempty"`
	GlobalTaintLevel types.TaintLevel `json:"global_taint_level"`
	HandoffNodeID    string           `json:"handoff_node_id,omitempty"`
	NamespaceID      string           `json:"namespace_id,omitempty"`
	SnapshotAt       int64            `json:"snapshot_at"`
}

// buildHandoffResumeSnapshot 序列化当前执行期上下文为 JSON 字符串，供
// S_AWAIT_AGENT 分支落盘（阶段04 A-02）。在 a.sCtx.Mu 保护下读取字段。
// 序列化失败或超出 handoffSnapshotMaxBytes 均返回 error；调用方在此情况下
// 应以空字符串写入 ResumeCtxJSON（退化为旧的"仅消除死锁"行为），而不是让
// 整个挂起流程失败——快照是无损续跑的增强，不是挂起本身的前提条件。
func (a *Agent) buildHandoffResumeSnapshot() (string, error) {
	a.sCtx.Mu.RLock()
	snap := HandoffResumeContext{
		SchemaVersion:    HandoffResumeContextVersion,
		DAGModel:         a.sCtx.DAGModel,
		ExecuteResult:    a.sCtx.ExecuteResult,
		CompletedNodeIDs: a.sCtx.CompletedNodeIDs,
		GlobalTaintLevel: a.sCtx.GlobalTaintLevel,
		HandoffNodeID:    a.sCtx.HandoffTaskID,
		NamespaceID:      a.sCtx.NamespaceID,
		SnapshotAt:       time.Now().Unix(),
	}
	a.sCtx.Mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "buildHandoffResumeSnapshot: marshal failed", err)
	}
	if len(data) > handoffSnapshotMaxBytes {
		metrics.RecordAgentHandoffSnapshotOversized(context.Background())
		return "", apperr.New(apperr.CodeResourceExhausted,
			"buildHandoffResumeSnapshot: snapshot exceeds handoffSnapshotMaxBytes")
	}
	return string(data), nil
}

// persistHandoffWaitCheckpoint 落盘 S_AWAIT_AGENT 挂起的 checkpoint 行（GD-1
// 基本死锁消除保证）+ 执行期上下文快照（GD-13-009 无损续跑增强）。从
// executeDeterministicEffect 的 S_AWAIT_AGENT 分支拆出（nestif 治理，行为
// 不变）：a.taskCheckpointRepo 为 nil 时直接跳过（无持久化能力，仅内存态
// watcher 生效，与拆分前一致）。
func (a *Agent) persistHandoffWaitCheckpoint(ctx context.Context) {
	if a.taskCheckpointRepo == nil {
		return
	}
	// GD-13-009：序列化执行期上下文快照（DAGModel/ExecuteResult/
	// CompletedNodeIDs/GlobalTaintLevel），供进程重启后 ResumeAwaitingHandoff
	// 无损续跑。序列化失败/超体积上限时 resumeJSON 为空字符串——checkpoint 行
	// 仍然落盘（消除死锁的基本保证不受影响），只是退化为旧的"仅消除死锁"
	// 恢复行为。
	resumeJSON, serr := a.buildHandoffResumeSnapshot()
	if serr != nil {
		slog.Error("kernel: build handoff resume snapshot failed, "+
			"resume will be degraded to deadlock-avoidance only",
			"session_id", a.sCtx.SessionID, "err", serr)
	}
	err := a.taskCheckpointRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
		TaskID:        a.sCtx.SessionID,
		NodeID:        a.sCtx.HandoffTaskID,
		Attempt:       1,
		Status:        "await_agent",
		StartedAt:     time.Now().Unix(),
		Reason:        "handoff_wait",
		TaintLevel:    a.sCtx.GlobalTaintLevel,
		ResumeCtxJSON: resumeJSON,
	})
	if err != nil {
		slog.Error("kernel: persist handoff wait failed", "err", err)
	}
}

// ResumeAwaitingHandoff 供 AwaitingHandoffReconciler（GD-13-003）在进程重启后
// 使用：把刚从 Pool 新建、默认停在 S_IDLE 的 Agent 直接就位到崩溃前的
// S_AWAIT_AGENT 委派等待点，并（在快照可用时）回填执行期上下文，实现委派
// 节点下游 DAG 的无损续跑（GD-13-009）。复用 fsm.StateMachine 既有的
// ForceState（跳过 Trigger 边校验但记录 history，语义与其它致命异常强制切态
// 一致），不新增第二套强制切态机制。
//
// 安全边界：resumeCtxJSON 来自数据库，属于"跨越信任边界的反序列化输入"。
// 即便它源自本进程此前写入，也必须假定可能被直接改库篡改（XR/HE-2 不得因
// "数据来自自家表"而放行）。因此回填后强制重跑 S_VALIDATE 四层校验，校验
// 不通过则丢弃 DAG 并降级为"仅消除死锁"的旧行为。
//
// 返回值 restored 表示是否成功恢复了执行期上下文；false 时调用方必须记录
// 明确的降级日志，禁止把这类会话终态误判为正常完整完成。
func (a *Agent) ResumeAwaitingHandoff(childTaskID, resumeCtxJSON string) (restored bool) {
	a.sCtx.HandoffTaskID = childTaskID
	a.sm.ForceState(types.AgentStateAwaitAgent)

	if resumeCtxJSON == "" {
		// 无快照可用（旧版本写入的行、或 buildHandoffResumeSnapshot 当时失败）：
		// 走旧行为——只消除死锁，不做无损续跑。
		return false
	}

	var snap HandoffResumeContext
	if err := json.Unmarshal([]byte(resumeCtxJSON), &snap); err != nil {
		slog.Warn("agent: ResumeAwaitingHandoff snapshot unmarshal failed, degrading to deadlock-avoidance only",
			"session_id", a.sCtx.SessionID, "child_task_id", childTaskID, "err", err)
		return false
	}
	if snap.SchemaVersion != HandoffResumeContextVersion {
		// 不按当前结构强行解析——字段错位会产出"看似合法"的错误 DAG。
		slog.Warn("agent: ResumeAwaitingHandoff snapshot schema version mismatch, degrading to deadlock-avoidance only",
			"session_id", a.sCtx.SessionID, "child_task_id", childTaskID,
			"snapshot_version", snap.SchemaVersion, "expected_version", HandoffResumeContextVersion)
		return false
	}
	if snap.DAGModel == nil || len(snap.DAGModel.Nodes) == 0 {
		// 空 DAG 快照没有可续跑的下游节点，等同于旧行为，无需走校验流程。
		return false
	}

	// 强制重校验：反序列化的 DAG 必须重新通过与正常路径完全相同的四层校验
	// 管线，禁止假定"数据来自自家表就一定安全"（XR/HE-2）。
	plan := &protocol.DAGPlan{Nodes: snap.DAGModel.Nodes, Edges: snap.DAGModel.Edges}
	if a.dagValidator != nil {
		vCtx := &protocol.DAGValidationContext{
			Plan:             plan,
			ActiveTaintLevel: maxNodeTaintLevel(plan),
			PolicyGate:       a.Security.PolicyGate,
			ToolExecutor:     a.toolRegistry,
			AgentID:          a.sCtx.AgentID,
			SessionID:        a.sCtx.SessionID,
			SystemTier:       a.Config.SystemTier,
			ReviewChecker:    a.Security.TaintReviewChecker,
		}
		if verr := a.dagValidator.Validate(context.Background(), vCtx); verr != nil {
			slog.Warn("agent: ResumeAwaitingHandoff snapshot DAG failed S_VALIDATE re-check, discarding and degrading to deadlock-avoidance only",
				"session_id", a.sCtx.SessionID, "child_task_id", childTaskID, "err", verr)
			return false
		}
	} else {
		// fail-closed：无校验引擎时不敢回填一个未经校验的 DAG（与 runValidateDAG
		// 的 dagValidator==nil 分支同一原则）。
		slog.Warn("agent: ResumeAwaitingHandoff dagValidator is nil, cannot re-validate snapshot DAG, degrading to deadlock-avoidance only",
			"session_id", a.sCtx.SessionID, "child_task_id", childTaskID)
		return false
	}

	a.sCtx.Mu.Lock()
	a.sCtx.DAGModel = snap.DAGModel
	a.sCtx.ExecuteResult = snap.ExecuteResult
	a.sCtx.CompletedNodeIDs = snap.CompletedNodeIDs
	// 污点 only-up：禁止用快照值直接覆盖当前值（防降级攻击，ADR-0007）。
	if snap.GlobalTaintLevel > a.sCtx.GlobalTaintLevel {
		a.sCtx.GlobalTaintLevel = snap.GlobalTaintLevel
	}
	if snap.NamespaceID != "" {
		a.sCtx.NamespaceID = snap.NamespaceID
	}
	a.sCtx.Mu.Unlock()

	return true
}
