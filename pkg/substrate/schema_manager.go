package substrate

import (
	"database/sql"
	"strconv"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

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
func (sm *SchemaManager) ApplyMigrations() error {
	for _, m := range sm.migrations {
		if m.Version <= sm.currentVersion {
			continue
		}
		_ = sm.BeginMigration(m.Version)
		if err := m.Up(nil); err != nil {
			return &MigrationError{m.Version, err.Error()}
		}
		sm.currentVersion = m.Version
		_ = sm.CompleteMigration()
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
		return perrors.New(perrors.CodeInternal,
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
		return err
	}
	_, _ = sm.db.Exec(
		"INSERT INTO sys_config(key,value) VALUES('migration_version',?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		strconv.Itoa(version),
	)
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
	return err
}

type MigrationError struct {
	Version int
	Detail  string
}

func (e *MigrationError) Error() string {
	return "migration v" + strconv.Itoa(e.Version) + " failed: " + e.Detail
}
