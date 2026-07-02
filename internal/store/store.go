package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO 依赖

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteStore 实现 protocol.Store，基于 modernc/sqlite（WAL 模式）。
// 架构文档: docs/arch/M02-Storage-Fabric.md §1.1
//
// 设计约束:
//   - MaxOpenConns=1：SQLite 写连接单实例，所有调用方共享同一 *sql.DB。
//     MaxOpenConns=1 + WAL busy_timeout=5000ms 保证写串行化，无需额外互斥锁。
//   - WAL + synchronous=NORMAL：读写不互斥，崩溃恢复安全
//   - kv_store 表：通用键值存储，供 M5/M10/M12 上层封装使用
//
// 写路径分层（均安全，无死锁风险）:
//   - MutationBus（高频/批量）: events / decision_log 走 DatabaseWriter 批量提交
//   - Store.Put/Txn（同步/中频）: M5 记忆层 / scheduler / eval store
//   - store.DB() 直接写（CAS/复杂 SQL）: Blackboard CAS / interface/server 配置管理
type SQLiteStore struct {
	db       *sql.DB
	path     string
	schemaFS fs.ReadDirFS // 注入的 schema 文件系统，便于测试替换
}

var _ protocol.Store = (*SQLiteStore)(nil)

// OpenSQLite 打开（或创建）SQLite 数据库，执行 WAL 初始化与 schema 迁移。
// schemaDir 为包含 *.sql 迁移文件的 fs.ReadDirFS（生产环境用 embed.FS）。
func OpenSQLite(path string, schemaDir fs.ReadDirFS) (*SQLiteStore, error) {
	// WAL 模式：读写不阻塞，busy_timeout 避免写锁争用
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "open sqlite", err)
	}
	// 单写者：与 MutationBus 约束对齐，禁止并发写
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &SQLiteStore{db: db, path: path, schemaFS: schemaDir}

	if err := s.runMigrations(); err != nil {
		db.Close()
		return nil, apperr.Wrap(apperr.CodeInternal, "schema migration", err)
	}
	return s, nil
}

// OpenSQLiteFromDir 便捷函数——以文件系统路径字符串打开数据库。
// 等同于 OpenSQLite(dbPath, os.DirFS(schemaDir).(fs.ReadDirFS))。
// 适用于 main 入口等无法传递 embed.FS 的场景。
func OpenSQLiteFromDir(dbPath, schemaDirPath string) (*SQLiteStore, error) {
	dirFS := os.DirFS(schemaDirPath)
	rfs, ok := dirFS.(fs.ReadDirFS)
	if !ok {
		// go 1.16+ os.DirFS 始终实现 ReadDirFS，此分支仅作防御
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("os.DirFS(%s) does not implement fs.ReadDirFS", schemaDirPath))
	}
	return OpenSQLite(dbPath, rfs)
}

// runMigrations 按文件名数字前缀升序执行尚未应用的 *.sql 迁移文件。
// 每个文件对应一个版本号（前三位数字）；每次迁移单独事务，崩溃恢复安全。
func (s *SQLiteStore) runMigrations() error { //nolint:gocyclo
	// schema_versions 元表：追踪已应用迁移
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_versions (
		version     INTEGER PRIMARY KEY,
		filename    TEXT NOT NULL,
		applied_at  TEXT NOT NULL
	)`); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "create schema_versions", err)
	}

	// kv_store 通用键值表（Store 接口的物理底层）
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS kv_store (
		key        BLOB PRIMARY KEY,
		value      BLOB NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "create kv_store", err)
	}

	// 读取已应用版本
	rows, err := s.db.Query("SELECT version FROM schema_versions ORDER BY version")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.runMigrations", err)
	}
	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.runMigrations", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.runMigrations", err)
	}

	if s.schemaFS == nil {
		return nil // 无 schema 目录（测试场景）
	}

	// 收集并排序迁移文件
	type mig struct {
		version  int
		filename string
		content  string
	}
	entries, err := s.schemaFS.ReadDir(".")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "read schema dir", err)
	}
	var pending []mig //nolint:prealloc
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var ver int
		if _, parseErr := fmt.Sscanf(e.Name(), "%03d", &ver); parseErr != nil {
			continue // 不符合命名规范跳过
		}
		if applied[ver] {
			continue
		}
		data, err := fs.ReadFile(s.schemaFS, e.Name())
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteStore.runMigrations", err)
		}
		pending = append(pending, mig{ver, e.Name(), string(data)})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })

	for _, m := range pending {
		tx, err := s.db.Begin()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("begin tx for %s", m.filename), err)
		}
		if _, err := tx.Exec(m.content); err != nil {
			tx.Rollback() //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("exec migration %s", m.filename), err)
		}
		if _, err := tx.Exec(
			"INSERT INTO schema_versions(version, filename, applied_at) VALUES(?,?,?)",
			m.version, m.filename, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			tx.Rollback() //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("record migration %s", m.filename), err)
		}
		if err := tx.Commit(); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("commit migration %s", m.filename), err)
		}
	}
	return nil
}

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

// LoadAllPreferences implements protocol.StoreExtPreferences.
func (s *SQLiteStore) LoadAllPreferences(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM preferences`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LoadAllPreferences", err)
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "LoadAllPreferences", err)
		}
		prefs[k] = v
	}
	return prefs, rows.Err()
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
	return tx.Commit()
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
	return tx.Commit()
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

// DB 暴露底层 *sql.DB（MaxOpenConns=1，WAL 模式）。
// 适用场景：
//   - MutationBus.DatabaseWriter（AI 核心数据批量写）
//   - SQLiteBlackboard（CAS 操作，需同步确认）
//   - pkg/gateway/server（配置管理 CRUD，复杂 SQL 无法走 KV 接口）
//
// 所有调用方共享同一实例，MaxOpenConns=1 保证写串行化，无需额外锁。
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// SQLQuerier 返回 SQLiteStore 作为 protocol.SQLQuerier 接口。
// 适合需要同时传递 protocol.Store 和 protocol.SQLQuerier 的场景，避免调用方持有 *sql.DB。
// @arch: docs/upgrade/repo-interface-migration.md 附录B
func (s *SQLiteStore) SQLQuerier() protocol.SQLQuerier {
	return s.db
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

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
func (it *sqliteIterator) Close() error  { return it.rows.Close() }

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

func (s *SQLiteStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

func (s *SQLiteStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

func (s *SQLiteStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}
