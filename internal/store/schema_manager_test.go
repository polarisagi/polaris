package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

func setupSchemaTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sys_config (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create sys_config: %v", err)
	}
	return db
}

// TestApplyMigrations_BeginMigrationFailure_AbortsBeforeUp_S02 验证阶段02修复：
// BeginMigration 标记写入失败必须阻断迁移执行，不得静默继续调用 Up()。
// 回归锚点：修复前 `_ = sm.BeginMigration(...)` 吞没错误，Up() 仍会被执行，
// 但崩溃状态标记从未落盘，Recover() 无法感知半途而废的迁移。
func TestApplyMigrations_BeginMigrationFailure_AbortsBeforeUp_S02(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	// 故意不创建 sys_config 表，令 BeginMigration 的 INSERT 必然失败。

	upCalled := false
	sm := NewSchemaManager(db, []Migration{
		{
			Version: 1,
			Up: func(tx Transaction) error {
				upCalled = true
				return nil
			},
		},
	})

	if err := sm.ApplyMigrations(); err == nil {
		t.Fatal("expected error when BeginMigration marker write fails, got nil")
	}
	if upCalled {
		t.Error("Up() 不应在 BeginMigration 失败后被调用")
	}
}

// TestApplyMigrations_CompleteMigrationFailure_ReturnsError_S02 验证阶段02修复：
// CompleteMigration 标记写入失败必须向上传播，不得静默返回成功。
// 回归锚点：修复前 `_ = sm.CompleteMigration()` 吞没错误，ApplyMigrations 会返回
// nil（视为成功），但 migration_status 仍停留在 in_progress，下次启动 Recover()
// 会误判为"上次崩溃"而阻断启动——即便这次迁移的 DDL 变更本身已经成功提交。
func TestApplyMigrations_CompleteMigrationFailure_ReturnsError_S02(t *testing.T) {
	db := setupSchemaTestDB(t)
	defer db.Close()

	sm := NewSchemaManager(db, []Migration{
		{
			Version: 1,
			Up: func(tx Transaction) error {
				// 迁移自身的 DDL：顺带删除 sys_config，模拟"迁移成功提交后，
				// CompleteMigration 的收尾写入才失败"这一时序。
				return tx.Exec("DROP TABLE sys_config")
			},
		},
	})

	if err := sm.ApplyMigrations(); err == nil {
		t.Fatal("expected error when CompleteMigration marker write fails, got nil")
	}
}

// TestBeginMigration_DiagWriteFailure_NonFatal_S02 验证 migration_version 诊断
// 字段写入失败为 L2（Warn + counter，不阻断）：BeginMigration 本身仍应返回 nil，
// 但必须计入 GlobalSchemaMigrationDiagWriteFailuresTotal，否则该失败完全不可观测。
func TestBeginMigration_DiagWriteFailure_NonFatal_S02(t *testing.T) {
	db := setupSchemaTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TRIGGER reject_diag BEFORE INSERT ON sys_config
		WHEN NEW.key = 'migration_version'
		BEGIN SELECT RAISE(ABORT, 'simulated diag write failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	sm := NewSchemaManager(db, nil)
	before := metrics.GlobalSchemaMigrationDiagWriteFailuresTotal.Load()

	if err := sm.BeginMigration(3); err != nil {
		t.Fatalf("BeginMigration 不应因诊断字段写入失败而返回错误: %v", err)
	}

	after := metrics.GlobalSchemaMigrationDiagWriteFailuresTotal.Load()
	if after != before+1 {
		t.Errorf("expected GlobalSchemaMigrationDiagWriteFailuresTotal += 1, got before=%d after=%d", before, after)
	}

	// migration_status 主状态字段应正常落盘（未受诊断字段失败影响）。
	var status string
	if err := db.QueryRow(`SELECT value FROM sys_config WHERE key='migration_status'`).Scan(&status); err != nil {
		t.Fatalf("query migration_status: %v", err)
	}
	if status != "in_progress" {
		t.Errorf("expected migration_status=in_progress, got %q", status)
	}
}
