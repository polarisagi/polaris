package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// NotesStore 实现
// ============================================================================
// DDL 权威源：internal/protocol/schema/023_notes.sql
// SQLNotesStore  — 生产路径，SQL 持久化，CAS 乐观锁
// InMemNotesStore — 测试/降级路径，进程内 map

// ─── SQLNotesStore ────────────────────────────────────────────────────────────

// SQLNotesStore 基于 notes 表的跨会话轻量笔记实现。
type SQLNotesStore struct {
	db protocol.SQLQuerier
}

func NewSQLNotesStore(db protocol.SQLQuerier) *SQLNotesStore {
	return &SQLNotesStore{db: db}
}

// Get 按 key 查询笔记；不存在或已过期则返回 (nil, nil)。
func (s *SQLNotesStore) Get(ctx context.Context, key string) (*types.Note, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT key, content, version, tags_json, updated_at, expires_at
		FROM notes
		WHERE key = ?
		  AND (expires_at IS NULL OR expires_at > ?)
	`, key, time.Now().Unix())
	n, err := scanNote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "notes_store: get", err)
	}
	return n, nil
}

// Set 写入或更新笔记。expectedVersion=-1 表示"不检查版本"（首次写入用）；
// 已有记录时传入当前 version，不匹配返回 CodeConflict。
// 默认过期时间：7 天；tags 为 nil 时保留已有 tags。
func (s *SQLNotesStore) Set(ctx context.Context, key, content string, tags []string, expectedVersion int) error {
	if key == "" {
		return apperr.New(apperr.CodeInvalidInput, "notes_store: key must not be empty")
	}
	if len(content) > 65536 {
		return apperr.New(apperr.CodeInvalidInput, "notes_store: content exceeds 64KB limit")
	}

	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		tagsJSON = []byte("[]")
	}
	now := time.Now().Unix()
	expiresAt := now + 7*24*3600
	id := fmt.Sprintf("note_%s_%d", key, now)
	sizeBytes := len(content)

	if expectedVersion < 0 {
		// 首次写入：INSERT OR IGNORE + unconditional UPDATE（保持向后兼容）
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO notes (id, key, content, version, size_bytes, created_at, updated_at, expires_at, tags_json)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				content    = excluded.content,
				version    = version + 1,
				size_bytes = excluded.size_bytes,
				updated_at = excluded.updated_at,
				expires_at = excluded.expires_at,
				tags_json  = excluded.tags_json
		`, id, key, content, sizeBytes, now, now, expiresAt, string(tagsJSON))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "notes_store: set", err)
		}
	} else {
		// CAS UPDATE：WHERE version = expectedVersion
		result, execErr := s.db.ExecContext(ctx, `
			UPDATE notes
			SET content = ?, tags_json = ?, version = version + 1, size_bytes = ?, updated_at = ?, expires_at = ?
			WHERE key = ? AND version = ?
		`, content, string(tagsJSON), sizeBytes, now, expiresAt, key, expectedVersion)
		if execErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "notes_store: cas update", execErr)
		}
		rows, _ := result.RowsAffected()
		if rows == 0 {
			return apperr.New(apperr.CodeConflict,
				fmt.Sprintf("notes_store: CAS conflict on key=%q (expected version %d)", key, expectedVersion))
		}
	}
	return nil
}

// Delete 删除指定 key 的笔记；不存在时静默返回。
func (s *SQLNotesStore) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notes WHERE key = ?", key)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "notes_store: delete", err)
	}
	return nil
}

// List 返回含有指定 tag 的所有笔记；tag 为空时返回全部未过期笔记。
func (s *SQLNotesStore) List(ctx context.Context, tag string) ([]types.Note, error) {
	now := time.Now().Unix()
	var (
		rows *sql.Rows
		err  error
	)
	if tag == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT key, content, version, tags_json, updated_at, expires_at
			FROM notes
			WHERE expires_at IS NULL OR expires_at > ?
			ORDER BY updated_at DESC
		`, now)
	} else {
		// JSON_EACH 需要 SQLite 3.38+，用 LIKE 近似匹配兼容 Tier-0
		rows, err = s.db.QueryContext(ctx, `
			SELECT key, content, version, tags_json, updated_at, expires_at
			FROM notes
			WHERE (expires_at IS NULL OR expires_at > ?)
			  AND tags_json LIKE ?
			ORDER BY updated_at DESC
		`, now, "%"+tag+"%")
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "notes_store: list", err)
	}
	defer rows.Close()

	var notes []types.Note
	for rows.Next() {
		n, scanErr := scanNote(rows)
		if scanErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notes_store: scan", scanErr)
		}
		notes = append(notes, *n)
	}
	return notes, rows.Err()
}

// ListByTask 返回关联到指定 taskID 的所有笔记。
// 实现：查询包含 "task:{taskID}" tag 的笔记。
func (s *SQLNotesStore) ListByTask(ctx context.Context, taskID string) ([]types.Note, error) {
	return s.List(ctx, "task:"+taskID)
}

// GC 删除已过期笔记，返回删除条数。
func (s *SQLNotesStore) GC(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM notes WHERE expires_at IS NOT NULL AND expires_at <= ?",
		time.Now().Unix())
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "notes_store: gc", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// scanNote 从 sql.Scanner（单行或游标行）读取 Note 字段。
func scanNote(s interface {
	Scan(dest ...any) error
}) (*types.Note, error) {
	n := &types.Note{}
	var tagsJSON string
	var updatedAt int64
	var expiresAtSQL sql.NullInt64
	if err := s.Scan(&n.Key, &n.Content, &n.Version, &tagsJSON, &updatedAt, &expiresAtSQL); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "scanNote", err)
	}
	n.UpdatedAt = time.Unix(updatedAt, 0)
	if expiresAtSQL.Valid {
		t := time.Unix(expiresAtSQL.Int64, 0)
		n.ExpiresAt = &t
	}
	if tagsJSON != "" && tagsJSON != "[]" {
		_ = json.Unmarshal([]byte(tagsJSON), &n.Tags)
	}
	return n, nil
}

// 编译期验证
var _ protocol.NotesStore = (*SQLNotesStore)(nil)

// ─── InMemNotesStore ──────────────────────────────────────────────────────────

// InMemNotesStore 纯内存实现，用于测试和降级场景。
type InMemNotesStore struct {
	mu    sync.Mutex
	notes map[string]*types.Note
}

func NewInMemNotesStore() *InMemNotesStore {
	return &InMemNotesStore{notes: make(map[string]*types.Note)}
}

func (s *InMemNotesStore) Get(_ context.Context, key string) (*types.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[key]
	if !ok {
		return nil, nil
	}
	if n.ExpiresAt != nil && time.Now().After(*n.ExpiresAt) {
		return nil, nil
	}
	// 深拷贝 Tags：*n 浅拷贝仍与内部 map 存储的 *Note 共享底层数组，调用方
	// 修改返回值的 Tags 会直接污染内部缓存（GR-5-003）。
	cp := *n
	if n.Tags != nil {
		cp.Tags = append([]string(nil), n.Tags...)
	}
	return &cp, nil
}

func (s *InMemNotesStore) Set(_ context.Context, key, content string, tags []string, expectedVersion int) error {
	if key == "" {
		return apperr.New(apperr.CodeInvalidInput, "notes_store: key must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if expectedVersion >= 0 {
		if existing, ok := s.notes[key]; ok {
			if existing.Version != expectedVersion {
				return apperr.New(apperr.CodeConflict, "notes_store: in-mem CAS conflict")
			}
		} else {
			// Expected a version but no existing record found
			return apperr.New(apperr.CodeConflict, "notes_store: in-mem CAS conflict (record not found)")
		}
	}

	v := 1
	if existing, ok := s.notes[key]; ok {
		v = existing.Version + 1
	}
	exp := time.Now().Add(7 * 24 * time.Hour)
	// 深拷贝调用方传入的 tags：直接持有调用方切片会导致调用方后续复用/修改
	// 该切片时反向污染内部存储（GR-5-003 同类问题的写入方向）。
	var tagsCopy []string
	if tags != nil {
		tagsCopy = append([]string(nil), tags...)
	}
	s.notes[key] = &types.Note{
		Key:       key,
		Content:   content,
		Version:   v,
		Tags:      tagsCopy,
		UpdatedAt: time.Now(),
		ExpiresAt: &exp,
	}
	return nil
}

func (s *InMemNotesStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.notes, key)
	return nil
}

func (s *InMemNotesStore) List(_ context.Context, tag string) ([]types.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var result []types.Note //nolint:prealloc
	for _, n := range s.notes {
		if n.ExpiresAt != nil && now.After(*n.ExpiresAt) {
			continue
		}
		if tag != "" {
			found := false
			for _, t := range n.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		cp := *n
		if n.Tags != nil {
			cp.Tags = append([]string(nil), n.Tags...)
		}
		result = append(result, cp)
	}
	return result, nil
}

// ListByTask 返回关联到指定 taskID 的所有笔记。
func (s *InMemNotesStore) ListByTask(ctx context.Context, taskID string) ([]types.Note, error) {
	return s.List(ctx, "task:"+taskID)
}

func (s *InMemNotesStore) GC(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	count := 0
	for k, n := range s.notes {
		if n.ExpiresAt != nil && now.After(*n.ExpiresAt) {
			delete(s.notes, k)
			count++
		}
	}
	return count, nil
}

// 编译期验证
var _ protocol.NotesStore = (*InMemNotesStore)(nil)
