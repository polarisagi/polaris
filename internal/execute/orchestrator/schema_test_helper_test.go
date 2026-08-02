package orchestrator

// 2026-08-02 新增：本包此前 setupTestDB（reaper_test.go）/
// setupPatternBlackboard（pattern_test.go）/newMockSQLiteDB
// （sqlite_blackboard_test.go）各自手工内联复制了一份 tasks/events/
// task_checkpoints 建表 DDL，与 internal/protocol/schema/ SSoT 之间没有
// 任何自动化同步机制——035_task_checkpoints.sql 新增 resume_ctx_json 列时
// 这些内联副本就曾"改了 SSoT、测试库没跟着改"，导致
// TestDebateExecutor_FullRoundTrip/TestStateGraphExecutor_CheckpointResume/
// TestMapReduceExecutor 直接报 "no such column"（见
// local_playground/upgrade/99-new-findings.md 阶段04 A-02 发现）。
//
// 本文件提供唯一的 schema-backed 测试 DB 构造入口，直接执行 SSoT 的 .sql
// 文件建表，未来任何 Schema 变更自动对测试生效，无需再手工同步第二份 DDL。
//
// 生产 PostTask/writeTaskEvent 均显式在 INSERT 语句里提供 created_at/
// updated_at/status 等 NOT NULL 列的值（见 sqlite_blackboard.go），不依赖
// 列 DEFAULT，因此改用无 DEFAULT 的真实 SSoT 建表对现有测试用例安全。

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

// schemaBackedTestFiles 是本包测试实际依赖的 SSoT 建表文件子集（tasks/events/
// task_checkpoints），按依赖顺序执行；无需应用全仓 32 个 schema 文件。
var schemaBackedTestFiles = []string{"001_events.sql", "007_tasks.sql", "035_task_checkpoints.sql"}

// newSchemaBackedDB 打开 :memory: SQLite 并应用 SSoT 建表 DDL。
func newSchemaBackedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	// in-memory SQLite 每个连接是独立的数据库，必须限制为单连接
	db.SetMaxOpenConns(1)

	for _, f := range schemaBackedTestFiles {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply schema %s: %v", f, err)
		}
	}
	return db
}
