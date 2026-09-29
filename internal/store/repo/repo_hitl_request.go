package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteHITLRequestRepository hitl_requests（044）的读写实现，供 automation/hitl 在消费端声明的接口使用。
type SQLiteHITLRequestRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteHITLRequestRepository(db protocol.SQLQuerier) *SQLiteHITLRequestRepository {
	return &SQLiteHITLRequestRepository{db: db}
}

const hitlRequestCols = `id, agent_id, session_id, checkpoint_type, risk_level, taint_level, prompt_json,
	status, decided_by, reason, response_json, deadline_ns, created_at, decided_at`

// Insert 写入一行。同 id 复发（如同一扩展重复发起安全审查）用 upsert 整行覆盖，
// 与改造前 KV Put 同 key 覆盖的语义一致；否则固定 ID 的调用方第二次审批会因主键冲突失败。
func (r *SQLiteHITLRequestRepository) Insert(ctx context.Context, row repo.HITLRequestRow) error {
	var decidedBy, respJSON, decidedAt any
	if row.DecidedBy != "" {
		decidedBy = row.DecidedBy
	}
	if row.ResponseJSON != "" {
		respJSON = row.ResponseJSON
	}
	if row.DecidedAtMs != 0 {
		decidedAt = row.DecidedAtMs
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO hitl_requests(`+hitlRequestCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.AgentID, row.SessionID, row.CheckpointType, row.RiskLevel, row.TaintLevel, row.PromptJSON,
		row.Status, decidedBy, row.Reason, respJSON, row.DeadlineNs, row.CreatedAtMs, decidedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.Insert", err)
	}
	return nil
}

// Decide 仅对 pending 行条件更新；返回是否命中。未命中 = 已被裁决/已超时/已对账为 orphaned/不存在，
// 调用方据此拒绝重复裁决，不再依赖"先删后查"。
func (r *SQLiteHITLRequestRepository) Decide(
	ctx context.Context, id, status, decidedBy, reason, responseJSON string,
) (bool, error) {
	var respJSON any
	if responseJSON != "" {
		respJSON = responseJSON
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE hitl_requests SET status=?, decided_by=?, reason=?, response_json=?, decided_at=?
		 WHERE id=? AND status='pending'`,
		status, decidedBy, reason, respJSON, time.Now().UnixMilli(), id)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.Decide", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.Decide rows", err)
	}
	return n > 0, nil
}

func (r *SQLiteHITLRequestRepository) Get(ctx context.Context, id string) (repo.HITLRequestRow, error) {
	row, err := scanHITLRequest(r.db.QueryRowContext(ctx,
		`SELECT `+hitlRequestCols+` FROM hitl_requests WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return repo.HITLRequestRow{}, apperr.New(apperr.CodeNotFound, "hitl request not found: "+id)
	}
	if err != nil {
		return repo.HITLRequestRow{}, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.Get", err)
	}
	return row, nil
}

// ListPending 按创建时间升序返回全部 pending 行。
func (r *SQLiteHITLRequestRepository) ListPending(ctx context.Context) ([]repo.HITLRequestRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+hitlRequestCols+` FROM hitl_requests WHERE status='pending' ORDER BY created_at, id`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.ListPending", err)
	}
	defer rows.Close()
	var out []repo.HITLRequestRow
	for rows.Next() {
		row, err := scanHITLRequest(rows)
		if err != nil {
			return out, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.ListPending scan", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return out, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.ListPending iterate", err)
	}
	return out, nil
}

// CountHumanApprovals 统计 (checkpoint_type, agent_id) 在 sinceMs 之后、且晚于该键最近一次
// **人工拒绝**的人工批准行数（GD-14-004 信任累积）。
//
// 只认 decided_by='human'：自动放行/信任降级/超时都不计，否则降级产生的"通过"会反过来
// 加固降级依据（正反馈）。人工拒绝的时间点不受 sinceMs 限制——窗口只约束批准，
// 拒绝一旦发生就永久压过更早的批准（清零语义）。
func (r *SQLiteHITLRequestRepository) CountHumanApprovals(
	ctx context.Context, checkpointType, agentID string, sinceMs int64,
) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM hitl_requests
		 WHERE checkpoint_type=? AND agent_id=? AND status='approved' AND decided_by='human'
		   AND decided_at >= ?
		   AND decided_at > COALESCE((SELECT MAX(decided_at) FROM hitl_requests
		         WHERE checkpoint_type=? AND agent_id=? AND status='denied' AND decided_by='human'), 0)`,
		checkpointType, agentID, sinceMs, checkpointType, agentID).Scan(&n)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteHITLRequestRepository.CountHumanApprovals", err)
	}
	return n, nil
}

type hitlRowScanner interface{ Scan(dest ...any) error }

func scanHITLRequest(s hitlRowScanner) (repo.HITLRequestRow, error) {
	var (
		row                 repo.HITLRequestRow
		decidedBy, respJSON sql.NullString
		decidedAt           sql.NullInt64
	)
	if err := s.Scan(&row.ID, &row.AgentID, &row.SessionID, &row.CheckpointType, &row.RiskLevel, &row.TaintLevel,
		&row.PromptJSON, &row.Status, &decidedBy, &row.Reason, &respJSON, &row.DeadlineNs, &row.CreatedAtMs,
		&decidedAt); err != nil {
		return repo.HITLRequestRow{}, err //nolint:wrapcheck // 调用方按 sql.ErrNoRows 判定并统一包装
	}
	row.DecidedBy = decidedBy.String
	row.ResponseJSON = respJSON.String
	row.DecidedAtMs = decidedAt.Int64
	return row, nil
}
