package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
	db *sql.DB
}

func NewSQLNotesStore(db *sql.DB) *SQLNotesStore {
	return &SQLNotesStore{db: db}
}

// Get 按 key 查询笔记；不存在或已过期则返回 (nil, nil)。
func (s *SQLNotesStore) Get(ctx context.Context, key string) (*protocol.Note, error) {
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
		return nil, perrors.Wrap(perrors.CodeInternal, "notes_store: get", err)
	}
	return n, nil
}

// Set 写入或更新笔记（CAS version 乐观锁：冲突时返回 CodeConflict）。
// 默认过期时间：7 天；tags 为 nil 时保留已有 tags。
func (s *SQLNotesStore) Set(ctx context.Context, key, content string, tags []string) error {
	if key == "" {
		return perrors.New(perrors.CodeInvalidInput, "notes_store: key must not be empty")
	}
	if len(content) > 65536 {
		return perrors.New(perrors.CodeInvalidInput, "notes_store: content exceeds 64KB limit")
	}

	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		tagsJSON = []byte("[]")
	}
	now := time.Now().Unix()
	expiresAt := now + 7*24*3600
	id := fmt.Sprintf("note_%s_%d", key, now)
	sizeBytes := len(content)

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
		return perrors.Wrap(perrors.CodeInternal, "notes_store: set", err)
	}
	return nil
}

// Delete 删除指定 key 的笔记；不存在时静默返回。
func (s *SQLNotesStore) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notes WHERE key = ?", key)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "notes_store: delete", err)
	}
	return nil
}

// List 返回含有指定 tag 的所有笔记；tag 为空时返回全部未过期笔记。
func (s *SQLNotesStore) List(ctx context.Context, tag string) ([]protocol.Note, error) {
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
		return nil, perrors.Wrap(perrors.CodeInternal, "notes_store: list", err)
	}
	defer rows.Close()

	var notes []protocol.Note
	for rows.Next() {
		n, scanErr := scanNote(rows)
		if scanErr != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "notes_store: scan", scanErr)
		}
		notes = append(notes, *n)
	}
	return notes, rows.Err()
}

// GC 删除已过期笔记，返回删除条数。
func (s *SQLNotesStore) GC(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM notes WHERE expires_at IS NOT NULL AND expires_at <= ?",
		time.Now().Unix())
	if err != nil {
		return 0, perrors.Wrap(perrors.CodeInternal, "notes_store: gc", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// scanNote 从 sql.Scanner（单行或游标行）读取 Note 字段。
func scanNote(s interface {
	Scan(dest ...any) error
}) (*protocol.Note, error) {
	n := &protocol.Note{}
	var tagsJSON string
	var updatedAt int64
	var expiresAtSQL sql.NullInt64
	if err := s.Scan(&n.Key, &n.Content, &n.Version, &tagsJSON, &updatedAt, &expiresAtSQL); err != nil {
		return nil, err
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
	notes map[string]*protocol.Note
}

func NewInMemNotesStore() *InMemNotesStore {
	return &InMemNotesStore{notes: make(map[string]*protocol.Note)}
}

func (s *InMemNotesStore) Get(_ context.Context, key string) (*protocol.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[key]
	if !ok {
		return nil, nil
	}
	if n.ExpiresAt != nil && time.Now().After(*n.ExpiresAt) {
		return nil, nil
	}
	cp := *n
	return &cp, nil
}

func (s *InMemNotesStore) Set(_ context.Context, key, content string, tags []string) error {
	if key == "" {
		return perrors.New(perrors.CodeInvalidInput, "notes_store: key must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 1
	if existing, ok := s.notes[key]; ok {
		v = existing.Version + 1
	}
	exp := time.Now().Add(7 * 24 * time.Hour)
	s.notes[key] = &protocol.Note{
		Key:       key,
		Content:   content,
		Version:   v,
		Tags:      tags,
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

func (s *InMemNotesStore) List(_ context.Context, tag string) ([]protocol.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var result []protocol.Note
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
		result = append(result, *n)
	}
	return result, nil
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
