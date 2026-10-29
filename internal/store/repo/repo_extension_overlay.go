package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// --- extension_catalog_overlay（目录覆盖层，ADR-0123）---
// 结构体/构造函数见 repo_extension.go。

// ReplaceCatalogOverlay 整份替换覆盖层并写入元数据（同一事务）：覆盖层是生成器的完整快照，
// 增量合并会让上游已删除的条目永久残留。
func (r *SQLiteExtensionRepository) ReplaceCatalogOverlay(ctx context.Context, rows []types.ExtCatalogOverlayRow, meta map[string]string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay begin", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_catalog_overlay`); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay delete", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO extension_catalog_overlay(catalog_id, src_hash, name_zh, summary_zh, description_zh, description_en, topics, departments, industries, life, featured)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay prepare", err)
	}
	defer stmt.Close()
	for _, o := range rows {
		if _, err := stmt.ExecContext(ctx, o.CatalogID, o.SrcHash, o.NameZh, o.SummaryZh, o.DescriptionZh, o.DescriptionEn,
			o.Topics, o.Departments, o.Industries, o.Life, o.Featured); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay insert "+o.CatalogID, err)
		}
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO extension_catalog_overlay_meta(key, value, updated_at) VALUES(?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'))
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, k, v); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay meta "+k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceCatalogOverlay commit", err)
	}
	return nil
}

// GetCatalogOverlayMeta 读取覆盖层元数据；键不存在返回空串。
func (r *SQLiteExtensionRepository) GetCatalogOverlayMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM extension_catalog_overlay_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetCatalogOverlayMeta", err)
	}
	return v, nil
}

// ListCatalogOverlay 返回全部覆盖层行，按 catalog_id 索引（网关目录接口叠加用）。
func (r *SQLiteExtensionRepository) ListCatalogOverlay(ctx context.Context) (map[string]types.ExtCatalogOverlayRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT catalog_id, src_hash, name_zh, summary_zh, description_zh, description_en, topics, departments, industries, life, featured, updated_at
		FROM extension_catalog_overlay`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListCatalogOverlay", err)
	}
	defer rows.Close()
	out := map[string]types.ExtCatalogOverlayRow{}
	for rows.Next() {
		var o types.ExtCatalogOverlayRow
		if err := rows.Scan(&o.CatalogID, &o.SrcHash, &o.NameZh, &o.SummaryZh, &o.DescriptionZh, &o.DescriptionEn,
			&o.Topics, &o.Departments, &o.Industries, &o.Life, &o.Featured, &o.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListCatalogOverlay scan", err)
		}
		out[o.CatalogID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListCatalogOverlay rows", err)
	}
	return out, nil
}
