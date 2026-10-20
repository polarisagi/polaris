package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"database/sql"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"
)

type SQLiteSystemRepository struct {
	db *sql.DB
}

var _ repo.SystemRepository = (*SQLiteSystemRepository)(nil)

func NewSQLiteSystemRepository(db *sql.DB) *SQLiteSystemRepository {
	return &SQLiteSystemRepository{db: db}
}

func (r *SQLiteSystemRepository) UpsertPreference(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO preferences(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) DeletePreference(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM preferences WHERE key=?`, key)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) UpsertKV(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))`,
		key, value)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) RestoreKV(ctx context.Context, key, value, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO kv_store(key, value, updated_at) VALUES(?,?,?)`,
		key, value, updatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) UpsertVFSRef(ctx context.Context, vfsURI string, blobSize int64, createdAt int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sys_vfs_references (vfs_ref, ref_count, blob_size, created_at)
		VALUES (?, 1, ?, ?)
		ON CONFLICT(vfs_ref) DO UPDATE SET ref_count = ref_count + 1
	`, vfsURI, blobSize, createdAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteSystemRepository.UpsertVFSRef", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) GetPreference(ctx context.Context, key string) (string, error) {
	var val string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM preferences WHERE key = ?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteSystemRepository.GetPreference", err)
	}
	return val, nil
}

func (r *SQLiteSystemRepository) ListPreferences(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT key, value FROM preferences")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteSystemRepository.ListPreferences", err)
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteSystemRepository.ListPreferences scan", err)
		}
		prefs[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteSystemRepository.ListPreferences rows", err)
	}
	return prefs, nil
}

// GetPermissionMode 读取当前权限模式（每次工具调用都应现读，用户中途切换即刻生效）。
//
// 迁移：permission_mode 为空而旧键 computer_use_mode 有值时回落旧值并回写新键，此后
// 全部代码只读新键。未设置 → auto_review（产品默认）；值非法 → default（fail-closed
// 取最严档，而不是放宽到默认档）；读库失败同样返回 default 与错误。
func (r *SQLiteSystemRepository) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	val, err := r.GetPreference(ctx, types.PrefPermissionMode)
	if err != nil {
		return types.ModeDefault, apperr.Wrap(apperr.CodeInternal, "get permission_mode", err)
	}
	if val == "" {
		if val, err = r.migrateLegacyPermissionMode(ctx); err != nil {
			return types.ModeDefault, err
		}
	}
	if val == "" {
		return types.ModeAutoReview, nil
	}
	m, ok := types.ParsePermissionMode(val)
	if !ok {
		slog.WarnContext(ctx, "permission_mode: invalid stored value, falling back to strictest mode", "value", val)
		return types.ModeDefault, nil
	}
	return m, nil
}

// migrateLegacyPermissionMode 读取旧键 computer_use_mode；值合法时回写到新键，返回旧值（可能为空）。
func (r *SQLiteSystemRepository) migrateLegacyPermissionMode(ctx context.Context) (string, error) {
	legacy, err := r.GetPreference(ctx, types.LegacyPrefComputerUseMode)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "get legacy computer_use_mode", err)
	}
	if _, ok := types.ParsePermissionMode(legacy); legacy != "" && ok {
		if uerr := r.UpsertPreference(ctx, types.PrefPermissionMode, legacy); uerr != nil {
			slog.WarnContext(ctx, "permission_mode: migrate legacy computer_use_mode failed", "err", uerr)
		}
	}
	return legacy, nil
}

func (r *SQLiteSystemRepository) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	if _, ok := types.ParsePermissionMode(string(mode)); !ok {
		return apperr.New(apperr.CodeInvalidInput, "invalid permission mode: "+string(mode))
	}
	return r.UpsertPreference(ctx, types.PrefPermissionMode, string(mode))
}
