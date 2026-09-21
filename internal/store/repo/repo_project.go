package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteProjectRepository 实现 protorepo.ProjectRepository（ADR-0097）。
type SQLiteProjectRepository struct {
	db *sql.DB
}

var _ protorepo.ProjectRepository = (*SQLiteProjectRepository)(nil)

func NewSQLiteProjectRepository(db *sql.DB) *SQLiteProjectRepository {
	return &SQLiteProjectRepository{db: db}
}

const projectCols = `id, name, root_path, instructions, trusted, archived, created_at, updated_at`

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// validateDefaultProjectImmutable 默认项目只允许改名。
//
// 为什么：channel / 无头会话恒落默认项目；若默认项目可绑目录、置信任或带指令，
// 任何一个 channel 用户的会话都会随之获得该目录的（可信）上下文——把 ADR-0097
// 决策二收窄的写入面从另一侧重新打开。
func validateDefaultProjectImmutable(p types.ProjectRow) error {
	if p.RootPath != "" || p.Trusted || p.Archived || p.Instructions != "" {
		return apperr.New(apperr.CodeInvalidInput, "默认项目仅允许修改名称（不可绑定目录/信任/归档/指令）")
	}
	return nil
}

func (r *SQLiteProjectRepository) CreateProject(ctx context.Context, p types.ProjectRow) error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
		return apperr.New(apperr.CodeInvalidInput, "project id/name 不能为空")
	}
	if p.ID == protorepo.DefaultProjectID {
		return apperr.New(apperr.CodeAlreadyExists, "默认项目已存在")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO projects(`+projectCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.RootPath, p.Instructions, b2i(p.Trusted), b2i(p.Archived), now, now)
	if err != nil {
		// 只认 UNIQUE：CHECK 约束失败不是"已存在"。
		if strings.Contains(err.Error(), "UNIQUE") {
			return apperr.Wrap(apperr.CodeAlreadyExists, "project 已存在", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.CreateProject", err)
	}
	return nil
}

func scanProject(sc interface{ Scan(dest ...any) error }, p *types.ProjectRow) error {
	var trusted, archived int
	if err := sc.Scan(&p.ID, &p.Name, &p.RootPath, &p.Instructions, &trusted, &archived, &p.CreatedAt, &p.UpdatedAt); err != nil {
		// apperr 实现了 Unwrap，调用方仍可 errors.Is(err, sql.ErrNoRows)。
		return apperr.Wrap(apperr.CodeInternal, "scanProject", err)
	}
	p.Trusted = trusted == 1
	p.Archived = archived == 1
	return nil
}

func (r *SQLiteProjectRepository) GetProject(ctx context.Context, id string) (*types.ProjectRow, error) {
	var p types.ProjectRow
	err := scanProject(r.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id=?`, id), &p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.Wrap(apperr.CodeNotFound, "project not found", err)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.GetProject", err)
	}
	// 与 ListProjects 同口径填 SessionCount，否则单条接口恒返回 0，客户端无从区分"空项目"。
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_sessions WHERE project_id=?`, id).Scan(&p.SessionCount); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.GetProject count", err)
	}
	return &p, nil
}

func (r *SQLiteProjectRepository) ListProjects(ctx context.Context, includeArchived bool) ([]types.ProjectRow, error) {
	q := `SELECT p.id, p.name, p.root_path, p.instructions, p.trusted, p.archived, p.created_at, p.updated_at,
	             COUNT(cs.id) AS session_count
	      FROM projects p LEFT JOIN chat_sessions cs ON cs.project_id = p.id`
	if !includeArchived {
		q += ` WHERE p.archived = 0`
	}
	q += ` GROUP BY p.id ORDER BY (p.id = 'default') DESC, p.updated_at DESC, p.id`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.ListProjects", err)
	}
	defer rows.Close()

	out := []types.ProjectRow{}
	for rows.Next() {
		var p types.ProjectRow
		var trusted, archived int
		if err := rows.Scan(&p.ID, &p.Name, &p.RootPath, &p.Instructions, &trusted, &archived,
			&p.CreatedAt, &p.UpdatedAt, &p.SessionCount); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.ListProjects scan", err)
		}
		p.Trusted = trusted == 1
		p.Archived = archived == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.ListProjects rows", err)
	}
	return out, nil
}

func (r *SQLiteProjectRepository) UpdateProject(ctx context.Context, p types.ProjectRow) error {
	if strings.TrimSpace(p.Name) == "" {
		return apperr.New(apperr.CodeInvalidInput, "project name 不能为空")
	}
	if p.ID == protorepo.DefaultProjectID {
		if err := validateDefaultProjectImmutable(p); err != nil {
			return err
		}
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE projects SET name=?, root_path=?, instructions=?, trusted=?, archived=?,
		        updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		p.Name, p.RootPath, p.Instructions, b2i(p.Trusted), b2i(p.Archived), p.ID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.UpdateProject", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "project not found")
	}
	return nil
}

func (r *SQLiteProjectRepository) DeleteProject(ctx context.Context, id string) error {
	if id == protorepo.DefaultProjectID {
		return apperr.New(apperr.CodeInvalidInput, "默认项目不可删除")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.DeleteProject begin", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 为 no-op

	if _, err := tx.ExecContext(ctx,
		`UPDATE chat_sessions SET project_id=? WHERE project_id=?`, protorepo.DefaultProjectID, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.DeleteProject move sessions", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.DeleteProject", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "project not found")
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.DeleteProject commit", err)
	}
	return nil
}

func (r *SQLiteProjectRepository) GetProjectBySession(ctx context.Context, sessionID string) (*types.ProjectRow, error) {
	var p types.ProjectRow
	err := scanProject(r.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.root_path, p.instructions, p.trusted, p.archived, p.created_at, p.updated_at
		 FROM chat_sessions cs JOIN projects p ON p.id = cs.project_id WHERE cs.id=?`, sessionID), &p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.GetProjectBySession", err)
	}
	return &p, nil
}

func (r *SQLiteProjectRepository) RestoreProject(ctx context.Context, p types.ProjectRow) error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
		return apperr.New(apperr.CodeInvalidInput, "project id/name 不能为空")
	}
	if p.ID == protorepo.DefaultProjectID {
		if _, err := r.db.ExecContext(ctx, `UPDATE projects SET name=? WHERE id=?`, p.Name, p.ID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.RestoreProject default", err)
		}
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if p.CreatedAt == "" {
		p.CreatedAt = now
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = now
	}
	// trusted 固定写 0，见接口注释。
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO projects(`+projectCols+`) VALUES(?,?,?,?,0,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, root_path=excluded.root_path,
		   instructions=excluded.instructions, trusted=0, archived=excluded.archived,
		   created_at=excluded.created_at, updated_at=excluded.updated_at`,
		p.ID, p.Name, p.RootPath, p.Instructions, b2i(p.Archived), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProjectRepository.RestoreProject", err)
	}
	return nil
}
