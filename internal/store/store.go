package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO 依赖

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteStore 实现 protocol.Store，基于 modernc/sqlite（WAL 模式）。
// 架构文档: docs/arch/M02-Storage-Fabric.md §1.1、§8
//
// 设计约束:
//   - 读写双连接池分离（同一 DB 文件，两个独立 *sql.DB 实例）：
//     writer: MaxOpenConns=1，所有写操作共享同一物理连接，
//   - WAL busy_timeout=5000ms 保证写串行化，无需额外互斥锁；
//     reader: MaxOpenConns>1（PRAGMA query_only=1 强制只读），
//     WAL 模式下多 reader 与单 writer 天然不互斥，避免只读查询
//     排队卡在写路径后面（历史教训：插件市场全量同步等长耗时批量
//     写占住唯一连接时，/v1/mcp-servers 等管理只读接口会无限期挂起）。
//   - kv_store 表：通用键值存储，供 M5/M10/M12 上层封装使用
//
// 写路径分层（均安全，无死锁风险，均落在 writer 连接上）:
//   - MutationBus（高频/批量）: events / decision_log 走 DatabaseWriter 批量提交
//   - Store.Put/Txn（同步/中频）: M5 记忆层 / scheduler / eval store
//   - store.DB() 直接写（CAS/复杂 SQL）: Blackboard CAS / interface/server 配置管理
//
// 读路径统一走 reader 连接（QueryContext/QueryRowContext/store.ReadDB()）：
// 网关管理只读接口（/v1/mcp-servers、/v1/channels 等）、MemoryAgent 等后台
// 扫描 Agent 均经由 protocol.Store/protocol.SQLQuerier 这一层间接落到 reader 连接，
// 不直接持有裸 *sql.DB，保持"统一存取入口"不变量（禁止模块自行 sql.Open）。
//
// Store 运行时方法（Get/Put/Scan/Txn/...）与内部 sqliteIterator/sqliteTx 见
// store_ops.go（R7 拆分）。
type SQLiteStore struct {
	db       *sql.DB // 写连接：MaxOpenConns=1，MutationBus 单写者
	readDB   *sql.DB // 读连接池：MaxOpenConns>1，query_only，供只读查询使用
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

	// 只读连接池：同一 DB 文件的独立 *sql.DB 实例，query_only=1 从引擎层禁止写入。
	// WAL 模式下多个 reader 与单个 writer 互不阻塞（读不等写、写不等读），
	// 用于承接 QueryContext/QueryRowContext（网关管理只读接口、MemoryAgent 等
	// 后台扫描），避免长耗时批量写（如插件市场全量同步）独占 writer 时
	// 把只读请求一并卡死。连接数保守设为 4：Tier-0（2GB VPS）内存/fd 约束下
	// 无需更大并发，且 SQLite 本身对写吞吐无收益。
	//
	// `:memory:` 特判：每次 sql.Open("sqlite", ":memory:") 都是彼此独立、
	// 不共享数据的内存库（无 cache=shared），若照常再开一个 readDB 会变成
	// "写进 writer 的数据、reader 永远读不到"（测试套件大量用 :memory: 构造
	// 一次性 store，不存在生产环境那种长耗时批量写占用连接的场景）。此时直接
	// 复用 writer 连接，读写分离退化为等价于旧行为，仅生产环境（真实文件路径）
	// 才真正拆分两个连接池。
	var readDB *sql.DB
	if path == ":memory:" {
		readDB = db
	} else {
		readDSN := fmt.Sprintf(
			"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)",
			path,
		)
		readDB, err = sql.Open("sqlite", readDSN)
		if err != nil {
			db.Close()
			return nil, apperr.Wrap(apperr.CodeInternal, "open sqlite (read pool)", err)
		}
		readDB.SetMaxOpenConns(4)
		readDB.SetMaxIdleConns(4)
		readDB.SetConnMaxLifetime(0)
	}

	s := &SQLiteStore{db: db, readDB: readDB, path: path, schemaFS: schemaDir}

	if err := s.runMigrations(); err != nil {
		if readDB != db {
			readDB.Close()
		}
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

	// kv_store 通用键值表（Store 接口的物理底层）。
	//
	// GR-1-001 复核结论：该建表语句与 schema_versions 一样保留硬编码在 Go 内，
	// 不迁移至 internal/protocol/schema/（不同于其余业务表）。原因：
	// OpenSQLite(path, nil) 是本包显式支持并测试覆盖的"无 schema 目录"轻量
	// KV-only 模式（见 store_test.go 全部用例），runMigrations 在 schemaFS==nil
	// 时提前 return（下方），若把 kv_store 迁移到 schema 文件，该模式下
	// kv_store 表将不再被创建，导致 Store.Get/Put/Delete/Scan 全部失败于
	// "no such table"。kv_store 与 schema_versions 同属两个跳过 SSoT 目录、
	// 必须在迁移系统自身可用之前/之外就绪的基础设施表，此为已核实的合理例外。
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
