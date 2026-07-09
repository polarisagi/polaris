package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// Get 读取键值。键不存在返回 apperr.ErrNotFound。
func (s *SQLiteStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	var val []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM kv_store WHERE key = ?", key,
	).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.ErrNotFound
	}
	if err != nil {
		return val, apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Get", err)
	}
	return val, nil
}

// Put 写入（或覆盖）键值。
// 同步写路径：适合低频、需要立即确认的操作（M5 记忆层、scheduler、eval store）。
// 高频批量写（events/decision_log）应走 MutationBus 以获得批量提交优化。
func (s *SQLiteStore) Put(ctx context.Context, key, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))",
		key, value,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Put", err)
	}
	return nil
}

func (s *SQLiteStore) Delete(ctx context.Context, key []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM kv_store WHERE key = ?", key)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Delete", err)
	}
	return nil
}

// Scan 返回前缀扫描迭代器；调用方须在使用完毕后调用 Close()。
// 使用范围查询（key >= prefix AND key < prefix_end）代替 LIKE，避免 BLOB 类型的 LIKE 匹配不可靠问题。
func (s *SQLiteStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	end := prefixSuccessor(prefix)
	var rows *sql.Rows
	var err error
	if end == nil {
		// 前缀全为 0xFF 的极端情况：无上界
		rows, err = s.db.QueryContext(ctx,
			"SELECT key, value FROM kv_store WHERE key >= ? ORDER BY key", prefix,
		)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT key, value FROM kv_store WHERE key >= ? AND key < ? ORDER BY key",
			prefix, end,
		)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Scan", err)
	}
	return &sqliteIterator{rows: rows}, nil
}

// ImportBackupRow 提供幂等upsert能力（备份恢复专用）。
func (s *SQLiteStore) ImportBackupRow(ctx context.Context, table string, row map[string]any) error {
	switch table {
	case "chat_sessions":
		id, _ := row["id"].(string)
		title, _ := row["title"].(string)
		thrashing, _ := row["thrashing_index"].(float64)
		createdAt, _ := row["created_at"].(string)
		updatedAt, _ := row["updated_at"].(string)
		if id == "" {
			return apperr.New(apperr.CodeInvalidInput, "missing session id")
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO chat_sessions(id, title, thrashing_index, created_at, updated_at) VALUES(?,?,?,?,?)`,
			id, title, thrashing, createdAt, updatedAt)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ImportBackupRow chat_sessions", err)
		}
		return nil

	case "chat_messages":
		id, _ := row["id"].(string)
		sessionID, _ := row["session_id"].(string)
		role, _ := row["role"].(string)
		content, _ := row["content"].(string)
		createdAt, _ := row["created_at"].(string)
		if id == "" || sessionID == "" {
			return apperr.New(apperr.CodeInvalidInput, "missing message id or session_id")
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO chat_messages(id, session_id, role, content, created_at) VALUES(?,?,?,?,?)`,
			id, sessionID, role, content, createdAt)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ImportBackupRow chat_messages", err)
		}
		return nil

	case "kv_store":
		key, _ := row["key"].(string)
		value, _ := row["value"].(string)
		updatedAt, _ := row["updated_at"].(string)
		if !strings.HasPrefix(key, "config:") {
			return apperr.New(apperr.CodeInvalidInput, "only config: kv allowed")
		}
		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO kv_store(key, value, updated_at) VALUES(?,?,?)`,
			key, value, updatedAt)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ImportBackupRow kv_store", err)
		}
		return nil

	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("ImportBackupRow: unknown table %s", table))
	}
}

// ListPreferences implements protocol.StoreExtPreferences.
func (s *SQLiteStore) ListPreferences(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM preferences`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ListPreferences", err)
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ListPreferences", err)
		}
		prefs[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ListPreferences: rows iteration", err)
	}
	return prefs, nil
}

// SetPreference implements protocol.StoreExtPreferences.
func (s *SQLiteStore) SetPreference(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preferences(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SetPreference", err)
	}
	return nil
}

// BatchWrite 批量原子写入；供迁移/初始化路径使用，业务路径走 MutationBus。
func (s *SQLiteStore) BatchWrite(ctx context.Context, ops []types.Op) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.BatchWrite", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, op := range ops {
		switch op.Type {
		case types.OpPut:
			if _, err := tx.ExecContext(ctx,
				"INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))",
				op.Key, op.Value,
			); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.BatchWrite", err)
			}
		case types.OpDelete:
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM kv_store WHERE key = ?", op.Key,
			); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.BatchWrite", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.BatchWrite: commit", err)
	}
	return nil
}

// Txn 在函数式事务中执行 fn；fn 返回错误自动 ROLLBACK，否则 COMMIT。
func (s *SQLiteStore) Txn(ctx context.Context, fn func(tx protocol.Transaction) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Txn", err)
	}
	stx := &sqliteTx{tx: tx}
	if err := fn(stx); err != nil {
		tx.Rollback() //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Txn", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Txn: commit", err)
	}
	return nil
}

func (s *SQLiteStore) Capabilities() types.StoreCapabilities {
	return types.StoreCapabilities{
		SupportsSQL:      true,
		SupportsVector:   false, // 向量/图/全文均路由至 [Storage-SurrealDB-Core]
		SupportsGraph:    false,
		SupportsFullText: false,
		Engine:           "sqlite-wal",
	}
}

// DB 暴露底层写连接 *sql.DB（MaxOpenConns=1，WAL 模式）。
// 适用场景：
//   - MutationBus.DatabaseWriter（AI 核心数据批量写）
//   - SQLiteBlackboard（CAS 操作，需同步确认）
//   - pkg/gateway/server（配置管理 CRUD，复杂 SQL 无法走 KV 接口）
//
// 所有调用方共享同一实例，MaxOpenConns=1 保证写串行化，无需额外锁。
// 仅需只读查询的调用方请改用 ReadDB()，避免占用写连接排队名额。
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// ReadDB 暴露只读连接池 *sql.DB（MaxOpenConns>1，query_only=1，WAL 模式）。
// 适用场景：网关管理只读接口（/v1/mcp-servers、/v1/channels、/v1/skills 等）、
// MemoryAgent 等后台扫描 Agent——凡是不需要与 writer 共享事务/CAS 语义的
// 纯查询场景都应优先使用此连接，避免被批量写操作（插件市场同步、向量回填等）
// 挤占唯一的写连接名额而无限期挂起。
// 该连接池已在引擎层禁止写入（query_only=1），误用 ExecContext 会直接报错。
func (s *SQLiteStore) ReadDB() *sql.DB { return s.readDB }

// SQLQuerier 返回 SQLiteStore 作为 protocol.SQLQuerier 接口。
// 适合需要同时传递 protocol.Store 和 protocol.SQLQuerier 的场景，避免调用方持有 *sql.DB。
// @arch: docs/upgrade/repo-interface-migration.md 附录B
func (s *SQLiteStore) SQLQuerier() protocol.SQLQuerier {
	return s.db
}

// Close 关闭读、写两个连接池。写连接优先关闭失败时仍尝试关闭读连接池，
// 两者错误用 errors.Join 合并后统一走 apperr 包装，避免读连接泄漏。
// `:memory:` 场景 readDB 与 db 是同一实例（见 OpenSQLite 注释），避免重复 Close。
func (s *SQLiteStore) Close() error {
	writeErr := s.db.Close()
	var readErr error
	if s.readDB != s.db {
		readErr = s.readDB.Close()
	}
	if joined := errors.Join(writeErr, readErr); joined != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.Close", joined)
	}
	return nil
}

// ─── sqliteIterator ───────────────────────────────────────────────────────────

type sqliteIterator struct {
	rows    *sql.Rows
	currKey []byte
	currVal []byte
	err     error
}

func (it *sqliteIterator) Next() bool {
	if !it.rows.Next() {
		it.err = it.rows.Err()
		return false
	}
	it.err = it.rows.Scan(&it.currKey, &it.currVal)
	return it.err == nil
}
func (it *sqliteIterator) Key() []byte   { return it.currKey }
func (it *sqliteIterator) Value() []byte { return it.currVal }
func (it *sqliteIterator) Err() error    { return it.err }
func (it *sqliteIterator) Close() error {
	if err := it.rows.Close(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqliteIterator.Close", err)
	}
	return nil
}

// ─── sqliteTx ─────────────────────────────────────────────────────────────────

type sqliteTx struct{ tx *sql.Tx }

func (t *sqliteTx) Get(key []byte) ([]byte, error) {
	var val []byte
	err := t.tx.QueryRow("SELECT value FROM kv_store WHERE key = ?", key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.ErrNotFound
	}
	if err != nil {
		return val, apperr.Wrap(apperr.CodeInternal, "sqliteTx.Get", err)
	}
	return val, nil
}

func (t *sqliteTx) Put(key, value []byte) error {
	_, err := t.tx.Exec(
		"INSERT OR REPLACE INTO kv_store(key, value, updated_at) VALUES(?,?,datetime('now'))",
		key, value,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqliteTx.Put", err)
	}
	return nil
}

func (t *sqliteTx) Delete(key []byte) error {
	_, err := t.tx.Exec("DELETE FROM kv_store WHERE key = ?", key)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqliteTx.Delete", err)
	}
	return nil
}

func (t *sqliteTx) Scan(prefix []byte) (protocol.Iterator, error) {
	end := prefixSuccessor(prefix)
	var rows *sql.Rows
	var err error
	if end == nil {
		rows, err = t.tx.Query(
			"SELECT key, value FROM kv_store WHERE key >= ? ORDER BY key", prefix,
		)
	} else {
		rows, err = t.tx.Query(
			"SELECT key, value FROM kv_store WHERE key >= ? AND key < ? ORDER BY key",
			prefix, end,
		)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sqliteTx.Scan", err)
	}
	return &sqliteIterator{rows: rows}, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// prefixSuccessor 返回前缀的返回大于该前缀的最小字节串（用于范围查询上界）。
// 若前缀全为 0xFF 则返回 nil（无上界）。
func prefixSuccessor(prefix []byte) []byte {
	succ := make([]byte, len(prefix))
	copy(succ, prefix)
	for i := len(succ) - 1; i >= 0; i-- {
		succ[i]++
		if succ[i] != 0 {
			return succ[:i+1]
		}
	}
	return nil // 前缀全为 0xFF，无上界
}

// ExecContext 写路径，落在 writer 连接（MaxOpenConns=1，MutationBus 单写者语义）。
func (s *SQLiteStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteStore.ExecContext", err)
	}
	return result, nil
}

// QueryContext / QueryRowContext 读路径，落在 readDB 只读连接池，
// 不与 writer 抢占同一连接名额（见 struct 注释 / ReadDB() 文档）。
func (s *SQLiteStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteStore.QueryContext", err)
	}
	return rows, nil
}

func (s *SQLiteStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.readDB.QueryRowContext(ctx, query, args...)
}
