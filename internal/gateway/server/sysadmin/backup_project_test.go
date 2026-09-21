package sysadmin

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite" // 013_chat.sql 含 FTS5，mattn 默认构建不带

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// newBackupTestDB 用真实 013_chat.sql 建库（避免手抄 DDL 与 SSoT 漂移），另补 kv_store。
func newBackupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply 013_chat.sql: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE kv_store (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func newBackupHandler(db *sql.DB) *SysAdminHandler {
	return &SysAdminHandler{
		DB:          db,
		ChatRepo:    repo.NewSQLiteChatRepository(db),
		ProjectRepo: repo.NewSQLiteProjectRepository(db),
		SystemRepo:  repo.NewSQLiteSystemRepository(db),
	}
}

// TestBackup_ProjectsRoundTrip 项目与会话归属随备份往返；trusted 不随备份迁移（ADR-0097）。
func TestBackup_ProjectsRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := newBackupTestDB(t)
	pr := repo.NewSQLiteProjectRepository(src)
	if err := pr.CreateProject(ctx, types.ProjectRow{ID: "prj_a", Name: "A", RootPath: "/tmp", Instructions: "先跑测试", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.NewSQLiteChatRepository(src).CreateSession(ctx, types.ChatSessionRow{ID: "s1", ProjectID: "prj_a"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	newBackupHandler(src).HandleExportBackup(w, httptest.NewRequest("GET", "/v1/export/backup", nil))
	dump := w.Body.Bytes()

	dst := newBackupTestDB(t)
	w = httptest.NewRecorder()
	newBackupHandler(dst).HandleImportBackup(w, httptest.NewRequest("POST", "/v1/import/backup", bytes.NewReader(dump)))

	p, err := repo.NewSQLiteProjectRepository(dst).GetProject(ctx, "prj_a")
	if err != nil {
		t.Fatalf("项目未恢复: %v (import resp=%s)", err, w.Body.String())
	}
	if p.Instructions != "先跑测试" || p.RootPath != "/tmp" {
		t.Fatalf("项目字段未恢复: %+v", p)
	}
	if p.Trusted {
		t.Fatal("trusted 不得随备份恢复")
	}
	sess, err := repo.NewSQLiteChatRepository(dst).GetSession(ctx, "s1")
	if err != nil || sess == nil || sess.ProjectID != "prj_a" {
		t.Fatalf("会话归属未恢复: %+v err=%v", sess, err)
	}
}

// TestBackup_LegacyBackupFallsBackToDefault 旧备份无 project_id / 引用不存在的项目 → 落默认项目，不丢会话。
func TestBackup_LegacyBackupFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	dst := newBackupTestDB(t)
	dump := `{"table":"__meta__","version":"1"}
{"table":"chat_sessions","row":{"id":"old","title":"t","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}
{"table":"chat_sessions","row":{"id":"orphan","title":"t","project_id":"prj_gone","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}}
`
	w := httptest.NewRecorder()
	newBackupHandler(dst).HandleImportBackup(w, httptest.NewRequest("POST", "/v1/import/backup", bytes.NewBufferString(dump)))
	cr := repo.NewSQLiteChatRepository(dst)
	for _, id := range []string{"old", "orphan"} {
		s, err := cr.GetSession(ctx, id)
		if err != nil || s == nil || s.ProjectID != "default" {
			t.Fatalf("%s: 应落默认项目，got %+v err=%v", id, s, err)
		}
	}
}
