package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteModelVersionRepository 实现 repo.ModelVersionRepository，
// 对应 internal/protocol/schema/033_model_version_registry.sql model_version_entries 表。
type SQLiteModelVersionRepository struct {
	db *sql.DB
}

var _ repo.ModelVersionRepository = (*SQLiteModelVersionRepository)(nil)

func NewSQLiteModelVersionRepository(db *sql.DB) *SQLiteModelVersionRepository {
	return &SQLiteModelVersionRepository{db: db}
}

const modelVersionSelectCols = `id, provider, model_id, version, deprecated, successor_model_id,
	prompt_template, tool_call_style, max_context, capabilities, validated_on,
	compatibility_score, consecutive_errors, updated_at`

// rowScanner 抽象 *sql.Row / *sql.Rows 共有的 Scan 方法，避免匿名接口（R-lint）。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanModelVersionEntry(scanner rowScanner) (*repo.ModelVersionEntry, error) {
	var e repo.ModelVersionEntry
	var deprecated int
	if err := scanner.Scan(
		&e.ID, &e.Provider, &e.ModelID, &e.Version, &deprecated, &e.SuccessorModelID,
		&e.PromptTemplate, &e.ToolCallStyle, &e.MaxContext, &e.Capabilities, &e.ValidatedOn,
		&e.CompatibilityScore, &e.ConsecutiveErrors, &e.UpdatedAt,
	); err != nil {
		return nil, err
	}
	e.Deprecated = deprecated != 0
	return &e, nil
}

func (r *SQLiteModelVersionRepository) Get(ctx context.Context, id string) (*repo.ModelVersionEntry, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+modelVersionSelectCols+" FROM model_version_entries WHERE id = ?", id)
	e, err := scanModelVersionEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.Get", err)
	}
	return e, nil
}

func (r *SQLiteModelVersionRepository) List(ctx context.Context) ([]*repo.ModelVersionEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+modelVersionSelectCols+" FROM model_version_entries ORDER BY provider, model_id")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.List", err)
	}
	defer rows.Close()
	return scanModelVersionRows(rows)
}

func (r *SQLiteModelVersionRepository) ListDeprecated(ctx context.Context) ([]*repo.ModelVersionEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+modelVersionSelectCols+" FROM model_version_entries WHERE deprecated = 1 ORDER BY provider, model_id")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.ListDeprecated", err)
	}
	defer rows.Close()
	return scanModelVersionRows(rows)
}

func (r *SQLiteModelVersionRepository) FindPredecessor(ctx context.Context, provider, modelID string) (*repo.ModelVersionEntry, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+modelVersionSelectCols+` FROM model_version_entries
		 WHERE provider = ? AND successor_model_id = ? LIMIT 1`, provider, modelID)
	e, err := scanModelVersionEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.FindPredecessor", err)
	}
	return e, nil
}

func (r *SQLiteModelVersionRepository) Upsert(ctx context.Context, e *repo.ModelVersionEntry) error {
	deprecated := 0
	if e.Deprecated {
		deprecated = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO model_version_entries (
			id, provider, model_id, version, deprecated, successor_model_id,
			prompt_template, tool_call_style, max_context, capabilities, validated_on,
			compatibility_score, consecutive_errors, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider=excluded.provider, model_id=excluded.model_id, version=excluded.version,
			deprecated=excluded.deprecated, successor_model_id=excluded.successor_model_id,
			prompt_template=excluded.prompt_template, tool_call_style=excluded.tool_call_style,
			max_context=excluded.max_context, capabilities=excluded.capabilities,
			validated_on=excluded.validated_on, compatibility_score=excluded.compatibility_score,
			consecutive_errors=excluded.consecutive_errors, updated_at=excluded.updated_at`,
		e.ID, e.Provider, e.ModelID, e.Version, deprecated, e.SuccessorModelID,
		e.PromptTemplate, e.ToolCallStyle, e.MaxContext, e.Capabilities, e.ValidatedOn,
		e.CompatibilityScore, e.ConsecutiveErrors, e.UpdatedAt,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.Upsert", err)
	}
	return nil
}

func (r *SQLiteModelVersionRepository) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM model_version_entries WHERE id = ?", id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteModelVersionRepository.Delete", err)
	}
	return nil
}

func scanModelVersionRows(rows *sql.Rows) ([]*repo.ModelVersionEntry, error) {
	var out []*repo.ModelVersionEntry
	for rows.Next() {
		e, err := scanModelVersionEntry(rows)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "scanModelVersionRows", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "scanModelVersionRows", err)
	}
	return out, nil
}
