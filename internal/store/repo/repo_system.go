package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"context"
	"database/sql"
	"fmt"

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
		return fmt.Errorf("db error: %w", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) DeletePreference(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM preferences WHERE key=?`, key)
	if err != nil {
		return fmt.Errorf("db error: %w", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) UpsertKV(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))`,
		key, value)
	if err != nil {
		return fmt.Errorf("db error: %w", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) RestoreKV(ctx context.Context, key, value, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO kv_store(key, value, updated_at) VALUES(?,?,?)`,
		key, value, updatedAt)
	if err != nil {
		return fmt.Errorf("db error: %w", err)
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
		return fmt.Errorf("SQLiteSystemRepository.UpsertVFSRef: %w", err)
	}
	return nil
}

func (r *SQLiteSystemRepository) GetPreference(ctx context.Context, key string) (string, error) {
	var val string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM preferences WHERE key = ?", key).Scan(&val)
	if err != nil {
		if err != nil {
			return "", fmt.Errorf("error: %w", err)
		}
		return "", nil
	}
	return val, nil
}

func (r *SQLiteSystemRepository) ListPreferences(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT key, value FROM preferences")
	if err != nil {
		if err != nil {
			return nil, fmt.Errorf("error: %w", err)
		}
		return nil, nil
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			if err != nil {
				return nil, fmt.Errorf("error: %w", err)
			}
			return nil, nil
		}
		prefs[k] = v
	}
	return prefs, rows.Err()
}

func (r *SQLiteSystemRepository) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	val, err := r.GetPreference(ctx, "permission_mode")
	if err != nil {
		return types.ModeAutoReview, fmt.Errorf("error: %w", err) // default
	}
	return types.PermissionMode(val), nil
}

func (r *SQLiteSystemRepository) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return r.UpsertPreference(ctx, "permission_mode", string(mode))
}
