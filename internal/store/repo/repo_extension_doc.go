package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// --- extension_doc_index & extension_doc_translation_tasks (ADR-0123 2026-11-14 复核) ---

// ReplaceDocIndex 整份替换说明文档集中索引并写入元数据（同一事务）。
func (r *SQLiteExtensionRepository) ReplaceDocIndex(ctx context.Context, rows []types.ExtDocIndexRow, meta map[string]string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex begin", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM extension_doc_index`); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex delete", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO extension_doc_index(
			catalog_id, marketplace_id, document_key,
			source_repo_url, source_revision, source_path, source_kind, source_sha256, source_base_url,
			catalog_revision, zh_path, zh_sha256, zh_origin, zh_review, zh_source_sha256,
			generator_version, provenance_path, rights_id, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'))`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex prepare", err)
	}
	defer stmt.Close()

	for _, d := range rows {
		if _, err := stmt.ExecContext(ctx,
			d.CatalogID, d.MarketplaceID, d.DocumentKey,
			d.SourceRepoURL, d.SourceRevision, d.SourcePath, d.SourceKind, d.SourceSHA256, d.SourceBaseURL,
			d.CatalogRevision, d.ZhPath, d.ZhSHA256, d.ZhOrigin, d.ZhReview, d.ZhSourceSHA256,
			d.GeneratorVersion, d.ProvenancePath, d.RightsID,
		); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex insert "+d.CatalogID, err)
		}
	}

	for k, v := range meta {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO extension_doc_index_meta(key, value, updated_at) VALUES(?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'))
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, k, v); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex meta "+k, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ReplaceDocIndex commit", err)
	}
	return nil
}

// GetDocIndexEntry 根据 catalog_id 查询集中文档索引行；不存在返回 nil, nil。
func (r *SQLiteExtensionRepository) GetDocIndexEntry(ctx context.Context, catalogID string) (*types.ExtDocIndexRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT catalog_id, marketplace_id, document_key,
			source_repo_url, source_revision, source_path, source_kind, source_sha256, source_base_url,
			catalog_revision, zh_path, zh_sha256, zh_origin, zh_review, zh_source_sha256,
			generator_version, provenance_path, rights_id, updated_at
		FROM extension_doc_index WHERE catalog_id=?`, catalogID)

	var d types.ExtDocIndexRow
	err := row.Scan(
		&d.CatalogID, &d.MarketplaceID, &d.DocumentKey,
		&d.SourceRepoURL, &d.SourceRevision, &d.SourcePath, &d.SourceKind, &d.SourceSHA256, &d.SourceBaseURL,
		&d.CatalogRevision, &d.ZhPath, &d.ZhSHA256, &d.ZhOrigin, &d.ZhReview, &d.ZhSourceSHA256,
		&d.GeneratorVersion, &d.ProvenancePath, &d.RightsID, &d.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetDocIndexEntry", err)
	}
	return &d, nil
}

// GetDocIndexMeta 读取文档索引元数据；键不存在返回空串。
func (r *SQLiteExtensionRepository) GetDocIndexMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM extension_doc_index_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetDocIndexMeta", err)
	}
	return v, nil
}

// UpsertDocTranslationTask 插入或更新翻译任务。
func (r *SQLiteExtensionRepository) UpsertDocTranslationTask(ctx context.Context, task types.ExtDocTranslationTaskRow) error {
	query := `INSERT INTO extension_doc_translation_tasks(
		task_id, principal_id, catalog_id, source_sha256, locale, generator_version,
		status, error_code, retry_after, attempts, result_cache_path, created_at, updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'),strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	ON CONFLICT(principal_id, catalog_id, source_sha256, locale, generator_version) DO UPDATE SET
		task_id=excluded.task_id,
		status=excluded.status,
		error_code=excluded.error_code,
		retry_after=excluded.retry_after,
		attempts=excluded.attempts,
		result_cache_path=excluded.result_cache_path,
		updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now')`

	_, err := r.db.ExecContext(ctx, query,
		task.TaskID, task.PrincipalID, task.CatalogID, task.SourceSHA256, task.Locale, task.GeneratorVersion,
		task.Status, task.ErrorCode, task.RetryAfter, task.Attempts, task.ResultCachePath,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertDocTranslationTask", err)
	}
	return nil
}

// GetDocTranslationTask 按 task_id 查询翻译任务；不存在返回 nil, nil。
func (r *SQLiteExtensionRepository) GetDocTranslationTask(ctx context.Context, taskID string) (*types.ExtDocTranslationTaskRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT task_id, principal_id, catalog_id, source_sha256, locale, generator_version,
			status, error_code, retry_after, attempts, result_cache_path, created_at, updated_at
		FROM extension_doc_translation_tasks WHERE task_id=?`, taskID)

	var t types.ExtDocTranslationTaskRow
	var errCode sql.NullString
	err := row.Scan(
		&t.TaskID, &t.PrincipalID, &t.CatalogID, &t.SourceSHA256, &t.Locale, &t.GeneratorVersion,
		&t.Status, &errCode, &t.RetryAfter, &t.Attempts, &t.ResultCachePath, &t.CreatedAt, &t.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetDocTranslationTask", err)
	}
	if errCode.Valid {
		t.ErrorCode = errCode.String
	}
	return &t, nil
}

// FindDocTranslationTask 按幂等键定位翻译任务；不存在返回 nil, nil。
func (r *SQLiteExtensionRepository) FindDocTranslationTask(ctx context.Context, principalID, catalogID, sourceSHA256, locale, generatorVersion string) (*types.ExtDocTranslationTaskRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT task_id, principal_id, catalog_id, source_sha256, locale, generator_version,
			status, error_code, retry_after, attempts, result_cache_path, created_at, updated_at
		FROM extension_doc_translation_tasks
		WHERE principal_id=? AND catalog_id=? AND source_sha256=? AND locale=? AND generator_version=?`,
		principalID, catalogID, sourceSHA256, locale, generatorVersion)

	var t types.ExtDocTranslationTaskRow
	var errCode sql.NullString
	err := row.Scan(
		&t.TaskID, &t.PrincipalID, &t.CatalogID, &t.SourceSHA256, &t.Locale, &t.GeneratorVersion,
		&t.Status, &errCode, &t.RetryAfter, &t.Attempts, &t.ResultCachePath, &t.CreatedAt, &t.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.FindDocTranslationTask", err)
	}
	if errCode.Valid {
		t.ErrorCode = errCode.String
	}
	return &t, nil
}

// UpdateDocTranslationTaskStatus 更新任务状态与执行结果。
func (r *SQLiteExtensionRepository) UpdateDocTranslationTaskStatus(ctx context.Context, taskID, status, errorCode string, retryAfter int, resultCachePath string) error {
	query := `UPDATE extension_doc_translation_tasks SET
		status=?,
		error_code=?,
		retry_after=?,
		result_cache_path=?,
		updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now')
	WHERE task_id=?`

	res, err := r.db.ExecContext(ctx, query, status, errorCode, retryAfter, resultCachePath, taskID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateDocTranslationTaskStatus", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return apperr.New(apperr.CodeNotFound, "translation task not found: "+taskID)
	}
	return nil
}

// ListPendingDocTranslationTasks 列出待处理或未完成的任务（重启恢复用）。
func (r *SQLiteExtensionRepository) ListPendingDocTranslationTasks(ctx context.Context, limit int) ([]types.ExtDocTranslationTaskRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT task_id, principal_id, catalog_id, source_sha256, locale, generator_version,
			status, error_code, retry_after, attempts, result_cache_path, created_at, updated_at
		FROM extension_doc_translation_tasks
		WHERE status IN ('queued', 'translating')
		ORDER BY created_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPendingDocTranslationTasks", err)
	}
	defer rows.Close()

	var list []types.ExtDocTranslationTaskRow
	for rows.Next() {
		var t types.ExtDocTranslationTaskRow
		var errCode sql.NullString
		if err := rows.Scan(
			&t.TaskID, &t.PrincipalID, &t.CatalogID, &t.SourceSHA256, &t.Locale, &t.GeneratorVersion,
			&t.Status, &errCode, &t.RetryAfter, &t.Attempts, &t.ResultCachePath, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPendingDocTranslationTasks scan", err)
		}
		if errCode.Valid {
			t.ErrorCode = errCode.String
		}
		list = append(list, t)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPendingDocTranslationTasks rows.Err", err)
	}
	return list, nil
}
