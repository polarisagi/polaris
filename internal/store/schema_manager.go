package store

import (
	"database/sql"
	"log/slog"
	"strconv"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// sqlTxWrapper 将 *sql.Tx 包装为 Transaction 接口，供 ApplyMigrations 注入迁移函数。
type sqlTxWrapper struct{ tx *sql.Tx }

func (w *sqlTxWrapper) Exec(query string, args ...any) error {
	_, err := w.tx.Exec(query, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sqlTxWrapper.Exec", err)
	}
	return nil
}

// SchemaManager — 版本化数据库迁移。
// 架构文档: docs/arch/02-Storage-Fabric-深度选型.md §4

type Migration struct {
	Version     int
	Description string
	Up          func(tx Transaction) error
	Down        func(tx Transaction) error
}

type SchemaManager struct {
	currentVersion int
	migrations     []Migration
	db             *sql.DB // 可选：用于 Recover() 状态检查
}

// Transaction 迁移事务接口。
type Transaction interface {
	Exec(query string, args ...any) error
}

// NewSchemaManager 创建带 DB 引用的 SchemaManager（db 可为 nil，降级为无状态模式）。
func NewSchemaManager(db *sql.DB, migrations []Migration) *SchemaManager {
	return &SchemaManager{db: db, migrations: migrations}
}

// ApplyMigrations 按 version 升序执行未应用迁移，每次迁移前后标记状态。
// db != nil 时每条迁移在独立事务内执行；db == nil 时传 nil Transaction（无状态模式）。
func (sm *SchemaManager) ApplyMigrations() error {
	for _, m := range sm.migrations {
		if m.Version <= sm.currentVersion {
			continue
		}
		// L1：迁移开始标记未落盘，崩溃后 Recover() 读不到 in_progress 状态，
		// 会让一次半途而废的 DDL 变更被当作"未开始"重复执行。必须 return。
		if err := sm.BeginMigration(m.Version); err != nil {
			return &MigrationError{m.Version, "begin migration marker: " + err.Error()}
		}

		var execErr error
		if sm.db != nil {
			execErr = sm.runMigrationInTx(m)
		} else {
			execErr = m.Up(nil)
		}

		if execErr != nil {
			return &MigrationError{m.Version, execErr.Error()}
		}
		sm.currentVersion = m.Version
		// L1：迁移完成标记未落盘，Recover() 会继续认为 in_progress，导致下次启动
		// 被误判为"上次崩溃"而阻断启动，即使迁移本身已成功提交。必须 return。
		if err := sm.CompleteMigration(); err != nil {
			return &MigrationError{m.Version, "complete migration marker: " + err.Error()}
		}
	}
	return nil
}

// runMigrationInTx 在独立事务内执行单条迁移，失败自动回滚，避免传 nil
// Transaction 导致 nil pointer panic。从 ApplyMigrations 抽出以降低嵌套深度
// （golangci-lint nestif 阈值）。
func (sm *SchemaManager) runMigrationInTx(m Migration) error {
	sqlTx, err := sm.db.Begin()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "begin tx", err)
	}
	if execErr := m.Up(&sqlTxWrapper{sqlTx}); execErr != nil {
		// L4：回滚失败无补救动作——事务已因 execErr 判定失败，回滚本身
		// 仅是尽力而为的资源释放；即便失败，连接池最终会因 tx 超时/关闭回收。
		if rbErr := sqlTx.Rollback(); rbErr != nil {
			slog.Debug("store/schema_manager: 迁移失败后事务回滚也失败，依赖连接释放兜底", "version", m.Version, "err", rbErr)
		}
		return execErr
	}
	if err := sqlTx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "commit tx", err)
	}
	return nil
}

// Recover 崩溃恢复：检查 sys_config.migration_status。
// in_progress 表示上次迁移在途中崩溃，返回错误阻止启动。
// 状态值：idle / in_progress / completed
func (sm *SchemaManager) Recover() error {
	if sm.db == nil {
		return nil
	}

	var status string
	err := sm.db.QueryRow(
		"SELECT value FROM sys_config WHERE key = 'migration_status' LIMIT 1",
	).Scan(&status)
	if err != nil {
		// ErrNoRows 或 sys_config 不存在 → 首次启动，正常
		return nil //nolint:nilerr
	}

	if status == "in_progress" {
		return apperr.New(apperr.CodeInternal,
			"schema_manager: incomplete migration detected (migration_status=in_progress) — "+
				"inspect DB and reset sys_config.migration_status='idle' before restarting")
	}
	return nil
}

// BeginMigration 迁移开始前将状态置为 in_progress。
func (sm *SchemaManager) BeginMigration(version int) error {
	if sm.db == nil {
		return nil
	}
	_, err := sm.db.Exec(
		"INSERT INTO sys_config(key,value) VALUES('migration_status','in_progress') " +
			"ON CONFLICT(key) DO UPDATE SET value='in_progress'",
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SchemaManager.BeginMigration", err)
	}
	// L2：migration_version 只是崩溃后人工排查"卡在哪个版本"的诊断辅助字段，
	// 不参与 Recover() 的状态机判定（该判定只读 migration_status）。写入失败
	// 不影响迁移正确性，但会削弱事故排查能力，故 Warn + counter 而非 return。
	if _, err := sm.db.Exec(
		"INSERT INTO sys_config(key,value) VALUES('migration_version',?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		strconv.Itoa(version),
	); err != nil {
		slog.Warn("store/schema_manager: 迁移版本诊断字段写入失败，不影响迁移正确性但会削弱崩溃排查能力", "version", version, "err", err)
		metrics.GlobalSchemaMigrationDiagWriteFailuresTotal.Add(1)
	}
	return nil
}

// CompleteMigration 迁移成功后将状态置为 completed。
func (sm *SchemaManager) CompleteMigration() error {
	if sm.db == nil {
		return nil
	}
	_, err := sm.db.Exec(
		"INSERT INTO sys_config(key,value) VALUES('migration_status','completed') " +
			"ON CONFLICT(key) DO UPDATE SET value='completed'",
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SchemaManager.CompleteMigration", err)
	}
	return nil
}

type MigrationError struct {
	Version int
	Detail  string
}

func (e *MigrationError) Error() string {
	return "migration v" + strconv.Itoa(e.Version) + " failed: " + e.Detail
}
