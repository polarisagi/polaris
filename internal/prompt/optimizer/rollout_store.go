package optimizer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// promptActivator 是 ConfirmShadow 通过后激活候选 Prompt 的窄接口。
// 由 *PromptVersionStore 实现（同包，见 version_store.go），避免直接持有具体类型
// 造成 SQLiteRolloutStore 在无 Prompt 场景（纯 L3/L4 候选）下的强依赖。
type promptActivator interface {
	Activate(ctx context.Context, taskType, id string, baselineScore float64) error
}

// SQLiteRolloutStore 实现 StagingPipeline 接口，State-in-DB。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.3
//
// Gate 语义:
//   Gate 0 (Pending):       SubmitCandidate → 等待 Eval 完成（eval_score 置位）
//   Gate 1 (Shadow 1%):     Eval 通过后自动进入，路由 1% 真实流量做影子对比
//   Gate 2 (Canary 5%):     Shadow 稳定 24h 后推进
//   Gate 3 (Canary 25%+):   按 canarySteps 逐步推进（25/50/100）
//   Gate 4 (Committed):     100% 流量切换，保留 7d 回滚窗口

const createRolloutTable = `
CREATE TABLE IF NOT EXISTS rollout_states (
	version          TEXT PRIMARY KEY,
	baseline         TEXT    NOT NULL,
	current_gate     INTEGER NOT NULL DEFAULT 0,
	canary_percent   INTEGER NOT NULL DEFAULT 0,
	status           TEXT    NOT NULL DEFAULT 'pending',
	eval_score       REAL    NOT NULL DEFAULT -1,
	shadow_ok        INTEGER NOT NULL DEFAULT 0,
	started_at       INTEGER NOT NULL,
	last_advanced_at INTEGER NOT NULL,
	metadata         TEXT    NOT NULL DEFAULT '{}'
)`

// SQLiteRolloutStore 持久化渐进发布状态。
type SQLiteRolloutStore struct {
	db              protocol.SQLQuerier
	rollout         *ProgressiveRollout
	promptActivator promptActivator // 可选：Gate 2(Shadow) 确认通过后回调激活 Prompt 候选
}

// NewSQLiteRolloutStore 创建 RolloutStore 并确保表存在。
func NewSQLiteRolloutStore(db protocol.SQLQuerier) (*SQLiteRolloutStore, error) {
	if _, err := db.ExecContext(context.Background(), createRolloutTable); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rollout_store: create table", err)
	}
	return &SQLiteRolloutStore{
		db:      db,
		rollout: NewProgressiveRollout(),
	}, nil
}

// WithPromptActivator 注入 Prompt 激活回调（*PromptVersionStore）。
// 未注入时 ConfirmShadow 仅推进 Gate 状态，不激活任何 Prompt（纯 L3/L4 候选场景）。
func (s *SQLiteRolloutStore) WithPromptActivator(a promptActivator) *SQLiteRolloutStore {
	s.promptActivator = a
	return s
}

// SubmitCandidate 提交新候选版本，直接进入 Gate 1 Shadow（1% 流量），状态 pending。
// Eval 通过 RecordEvalScore 异步补充；未调用时默认 eval_score=-1（不阻塞 Shadow 观察）。
func (s *SQLiteRolloutStore) SubmitCandidate(ctx context.Context, snap *AgentVersionSnapshot) error {
	now := time.Now().Unix()
	meta, _ := json.Marshal(snap)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rollout_states
			(version, baseline, current_gate, canary_percent, status, eval_score, shadow_ok, started_at, last_advanced_at, metadata)
		VALUES (?, 'baseline', 1, 1, 'pending', -1, 0, ?, ?, ?)
		ON CONFLICT(version) DO NOTHING
	`, snap.Version, now, now, string(meta))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rollout_store.SubmitCandidate", err)
	}
	return nil
}

// RecordEvalScore 记录 Eval 结果（异步补充评分，不阻塞 Shadow 观察）。
// passRate 不达标时触发回滚；达标时更新 eval_score 并将 status 设为 running。
func (s *SQLiteRolloutStore) RecordEvalScore(ctx context.Context, version string, passRate float64, baselinePassRate float64) error {
	// Eval 未通过（低于基线 × 0.95）→ 立即回滚
	if passRate < baselinePassRate*0.95 {
		_ = s.Rollback(ctx, version, fmt.Sprintf("eval regression: passRate=%.3f < baseline×0.95=%.3f", passRate, baselinePassRate*0.95))
		return nil
	}

	// 更新 eval_score 并激活 Shadow
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		UPDATE rollout_states
		SET eval_score = ?, status = 'running', last_advanced_at = ?
		WHERE version = ? AND status = 'pending'
	`, passRate, now, version)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.RecordEvalScore", err)
	}
	return nil
}

// ConfirmShadow 确认影子执行结果正常，允许从 Gate 1 推进到 Gate 2（Canary 5%）。
// 由影子执行监控组件（ShadowExecutor）在 shadow_ok 条件满足后调用。
// Gate 推进成功后，若已注入 promptActivator，进一步激活对应 Prompt 候选——这是
// M9 自进化真正生效的唯一入口，取代此前 handleEvalCompleted 内 Eval 一过就同步
// Activate、绕过 Shadow 验证的旧路径（见 docs/arch/decisions/ADR-0029 §K）。
func (s *SQLiteRolloutStore) ConfirmShadow(ctx context.Context, version string) error {
	state, err := s.GetState(ctx, version)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ConfirmShadow", err)
	}
	if state.CurrentGate != GateShadowExecution {
		return nil
	}

	var metaStr string
	if scanErr := s.db.QueryRowContext(ctx,
		`SELECT metadata FROM rollout_states WHERE version = ?`, version).Scan(&metaStr); scanErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ConfirmShadow: read metadata", scanErr)
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		UPDATE rollout_states
		SET shadow_ok = 1, current_gate = 2, canary_percent = 5, status = 'running', last_advanced_at = ?
		WHERE version = ?
	`, now, version)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ConfirmShadow", err)
	}

	if s.promptActivator != nil {
		var snap AgentVersionSnapshot
		if jsonErr := json.Unmarshal([]byte(metaStr), &snap); jsonErr == nil && snap.TaskType != "" {
			if actErr := s.promptActivator.Activate(ctx, snap.TaskType, version, snap.BaselineScore); actErr != nil {
				// Shadow 已确认通过，Gate 状态已推进；激活失败不回滚 Gate（避免重复触发
				// Shadow 回放），仅记录日志留待下次人工核查或候选自然被后续版本覆盖。
				slog.Warn("rollout_store: prompt activation after shadow confirm failed", "version", version, "err", actErr)
			}
		}
	}
	return nil
}

// ListPendingShadow 返回当前停留在 Gate 2(Shadow)、状态为 running 的候选版本号。
func (s *SQLiteRolloutStore) ListPendingShadow(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version FROM rollout_states WHERE current_gate = ? AND status = 'running'`,
		GateShadowExecution)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ListPendingShadow", err)
	}
	defer rows.Close()

	var versions []string
	for rows.Next() {
		var v string
		if scanErr := rows.Scan(&v); scanErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ListPendingShadow: scan", scanErr)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.ListPendingShadow: rows", err)
	}
	return versions, nil
}

// AdvanceGate 根据当前指标推进或触发硬停止。
// Gate 路径：
//
//	0 (Pending)       → 等待 RecordEvalScore 自动推进
//	1 (Shadow 1%)     → 等待 ConfirmShadow 推进到 Gate 2
//	2+ (Canary)       → 稳定 24h 后按 canarySteps 逐步推进到 100%
func (s *SQLiteRolloutStore) AdvanceGate(ctx context.Context, version string, stats RolloutStats) (*RolloutState, error) {
	state, err := s.GetState(ctx, version)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.AdvanceGate", err)
	}
	if state.Status == RolloutStatusRolledBack || state.Status == RolloutStatusCommitted {
		return state, nil
	}

	// 硬停止：任意指标超限立即回滚
	if s.rollout.CheckHardStop(stats) {
		if err := s.Rollback(ctx, version, "hard stop: metrics regression or safety violation"); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.AdvanceGate", err)
		}
		return s.GetState(ctx, version)
	}

	// Gate 1 Shadow：由 ConfirmShadow 推进到 Gate 2，此处跳过
	if state.CurrentGate <= GateShadowExecution {
		return state, nil
	}

	// Gate 2+：稳定期检查（24h）
	if time.Since(time.Unix(state.LastAdvancedAt, 0)) < 24*time.Hour {
		return state, nil
	}

	// 按 canarySteps 推进
	nextPercent, nextGate := s.rollout.NextStep(state.CanaryPercent, int(state.CurrentGate))
	newStatus := RolloutStatusRunning
	if nextPercent >= 100 {
		newStatus = RolloutStatusCommitted
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		UPDATE rollout_states
		SET current_gate = ?, canary_percent = ?, status = ?, last_advanced_at = ?
		WHERE version = ?
	`, nextGate, nextPercent, string(newStatus), now, version)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rollout_store.AdvanceGate", err)
	}

	return s.GetState(ctx, version)
}

// Rollback 将版本状态设为 rolled_back（实现 StagingPipeline 接口）。
func (s *SQLiteRolloutStore) Rollback(ctx context.Context, version string, reason string) error {
	meta := fmt.Sprintf(`{"rollback_reason":%q,"at":%d}`, reason, time.Now().Unix())
	_, err := s.db.ExecContext(ctx, `
		UPDATE rollout_states SET status = 'rolled_back', metadata = ? WHERE version = ?
	`, meta, version)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rollout_store.Rollback", err)
	}
	return nil
}

// GetState 从 SQLite 读取当前 RolloutState。
func (s *SQLiteRolloutStore) GetState(ctx context.Context, version string) (*RolloutState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT version, baseline, current_gate, canary_percent, status, started_at, last_advanced_at
		FROM rollout_states WHERE version = ?
	`, version)

	var st RolloutState
	var baseline string
	if err := row.Scan(&st.CandidateVersion, &baseline, &st.CurrentGate,
		&st.CanaryPercent, &st.Status, &st.StartedAt, &st.LastAdvancedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("rollout_store: version %q not found", version))
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteRolloutStore.GetState", err)
	}
	st.BaselineVersion = baseline
	return &st, nil
}

// NextStep 根据当前 CanaryPercent 返回下一步的 (percent, gate)。
func (pr *ProgressiveRollout) NextStep(currentPercent, currentGate int) (int, int) {
	for _, step := range pr.canarySteps {
		if step > currentPercent {
			return step, currentGate + 1
		}
	}
	return 100, currentGate + 1 // 全量
}
