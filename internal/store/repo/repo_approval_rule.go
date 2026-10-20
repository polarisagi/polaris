package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteApprovalRuleRepository 实现 protorepo.ApprovalRuleRepository。
type SQLiteApprovalRuleRepository struct {
	db *sql.DB
}

var _ protorepo.ApprovalRuleRepository = (*SQLiteApprovalRuleRepository)(nil)

func NewSQLiteApprovalRuleRepository(db *sql.DB) *SQLiteApprovalRuleRepository {
	return &SQLiteApprovalRuleRepository{db: db}
}

const approvalRuleCols = `id, project_id, kind, pattern, created_at, last_used_at, hit_count`

func validApprovalRuleKind(kind string) bool {
	switch kind {
	case "cmd", "domain", "dir", "app", "tool":
		return true
	}
	return false
}

func (r *SQLiteApprovalRuleRepository) AddRule(ctx context.Context, projectID, kind, pattern string) (types.ApprovalRuleRow, error) {
	pattern = strings.TrimSpace(pattern)
	if projectID == "" || pattern == "" || !validApprovalRuleKind(kind) {
		return types.ApprovalRuleRow{}, apperr.New(apperr.CodeInvalidInput, "approval rule: project/kind/pattern 非法")
	}
	row := types.ApprovalRuleRow{
		ID: uuid.NewString(), ProjectID: projectID, Kind: kind, Pattern: pattern,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO approval_rules(id, project_id, kind, pattern, created_at) VALUES(?,?,?,?,?)`,
		row.ID, row.ProjectID, row.Kind, row.Pattern, row.CreatedAt); err != nil {
		return types.ApprovalRuleRow{}, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.AddRule", err)
	}
	// 重复添加时 INSERT OR IGNORE 不生效，回读已有行保证幂等。
	got := r.db.QueryRowContext(ctx,
		`SELECT `+approvalRuleCols+` FROM approval_rules WHERE project_id=? AND kind=? AND pattern=?`,
		projectID, kind, pattern)
	out, err := scanApprovalRule(got)
	if err != nil {
		return types.ApprovalRuleRow{}, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.AddRule reload", err)
	}
	return out, nil
}

type approvalRuleScanner interface{ Scan(dest ...any) error }

func scanApprovalRule(s approvalRuleScanner) (types.ApprovalRuleRow, error) {
	var row types.ApprovalRuleRow
	err := s.Scan(&row.ID, &row.ProjectID, &row.Kind, &row.Pattern, &row.CreatedAt, &row.LastUsedAt, &row.HitCount)
	return row, err //nolint:wrapcheck // 调用方统一包装
}

func (r *SQLiteApprovalRuleRepository) ListRules(ctx context.Context, projectID string) ([]types.ApprovalRuleRow, error) {
	q := `SELECT ` + approvalRuleCols + ` FROM approval_rules`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	q += ` ORDER BY created_at, id`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.ListRules", err)
	}
	defer rows.Close() //nolint:errcheck // 只读游标
	var out []types.ApprovalRuleRow
	for rows.Next() {
		row, sErr := scanApprovalRule(rows)
		if sErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.ListRules scan", sErr)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.ListRules rows", err)
	}
	return out, nil
}

func (r *SQLiteApprovalRuleRepository) DeleteRule(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM approval_rules WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.DeleteRule", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "approval rule not found")
	}
	return nil
}

func (r *SQLiteApprovalRuleRepository) TouchRule(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE approval_rules SET hit_count=hit_count+1, last_used_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.TouchRule", err)
	}
	return nil
}

func (r *SQLiteApprovalRuleRepository) CountByProject(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT project_id, COUNT(*) FROM approval_rules GROUP BY project_id`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.CountByProject", err)
	}
	defer rows.Close() //nolint:errcheck // 只读游标
	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.CountByProject scan", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteApprovalRuleRepository.CountByProject rows", err)
	}
	return out, nil
}
