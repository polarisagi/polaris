package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// newProjectTestDB 用真实 013_chat.sql（schema SSoT）建库，而非手抄 DDL：
// 手抄会让"测试通过但真实 DDL 已漂移"成为可能。
func newProjectTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1) // :memory: 每连接独立空库
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply 013_chat.sql: %v", err)
	}
	return db
}

func TestProjectRepo_DefaultProjectSeeded(t *testing.T) {
	r := NewSQLiteProjectRepository(newProjectTestDB(t))
	p, err := r.GetProject(context.Background(), protorepo.DefaultProjectID)
	if err != nil {
		t.Fatalf("GetProject(default): %v", err)
	}
	if p.RootPath != "" || p.Trusted {
		t.Fatalf("默认项目须无目录且不信任: %+v", p)
	}
}

func TestProjectRepo_CreateGetUpdateList(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteProjectRepository(newProjectTestDB(t))

	if err := r.CreateProject(ctx, types.ProjectRow{ID: "prj_a", Name: "A", RootPath: "/tmp/a", Instructions: "先读 README", Trusted: true}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := r.CreateProject(ctx, types.ProjectRow{ID: "prj_a", Name: "dup"}); !apperr.IsCode(err, apperr.CodeAlreadyExists) {
		t.Fatalf("重复 ID 应返回 AlreadyExists，got %v", err)
	}
	got, err := r.GetProject(ctx, "prj_a")
	if err != nil || got.Name != "A" || got.RootPath != "/tmp/a" || !got.Trusted || got.Instructions != "先读 README" {
		t.Fatalf("GetProject: %+v err=%v", got, err)
	}

	got.Name, got.Trusted = "A2", false
	if err := r.UpdateProject(ctx, *got); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got2, _ := r.GetProject(ctx, "prj_a")
	if got2.Name != "A2" || got2.Trusted {
		t.Fatalf("更新未生效: %+v", got2)
	}

	if err := r.UpdateProject(ctx, types.ProjectRow{ID: "nope", Name: "x"}); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("更新不存在项目应 NotFound，got %v", err)
	}

	list, err := r.ListProjects(ctx, false)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListProjects: n=%d err=%v", len(list), err)
	}
	if list[0].ID != protorepo.DefaultProjectID {
		t.Fatalf("默认项目应排最前，got %s", list[0].ID)
	}
}

func TestProjectRepo_DefaultProjectImmutable(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteProjectRepository(newProjectTestDB(t))

	for name, p := range map[string]types.ProjectRow{
		"绑目录": {ID: "default", Name: "x", RootPath: "/tmp"},
		"置信任": {ID: "default", Name: "x", Trusted: true},
		"归档":  {ID: "default", Name: "x", Archived: true},
		"带指令": {ID: "default", Name: "x", Instructions: "i"},
	} {
		if err := r.UpdateProject(ctx, p); !apperr.IsCode(err, apperr.CodeInvalidInput) {
			t.Errorf("%s: 默认项目应拒绝，got %v", name, err)
		}
	}
	if err := r.UpdateProject(ctx, types.ProjectRow{ID: "default", Name: "我的默认"}); err != nil {
		t.Fatalf("默认项目改名应允许: %v", err)
	}
	if err := r.DeleteProject(ctx, protorepo.DefaultProjectID); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("删默认项目应拒绝，got %v", err)
	}
}

func TestProjectRepo_DeleteMovesSessionsToDefault(t *testing.T) {
	ctx := context.Background()
	db := newProjectTestDB(t)
	pr := NewSQLiteProjectRepository(db)
	cr := NewSQLiteChatRepository(db)

	if err := pr.CreateProject(ctx, types.ProjectRow{ID: "prj_x", Name: "X"}); err != nil {
		t.Fatal(err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s1", ProjectID: "prj_x"}); err != nil {
		t.Fatal(err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s2"}); err != nil { // 空 → default
		t.Fatal(err)
	}

	p, err := pr.GetProjectBySession(ctx, "s1")
	if err != nil || p == nil || p.ID != "prj_x" {
		t.Fatalf("s1 应属 prj_x: %+v err=%v", p, err)
	}
	if p, _ := pr.GetProjectBySession(ctx, "s2"); p == nil || p.ID != protorepo.DefaultProjectID {
		t.Fatalf("s2 应属 default: %+v", p)
	}
	if p, err := pr.GetProjectBySession(ctx, "ghost"); p != nil || err != nil {
		t.Fatalf("不存在会话应返回 (nil,nil): %+v %v", p, err)
	}

	if err := pr.DeleteProject(ctx, "prj_x"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if p, _ := pr.GetProjectBySession(ctx, "s1"); p == nil || p.ID != protorepo.DefaultProjectID {
		t.Fatalf("删项目后会话应迁回 default: %+v", p)
	}
	if err := pr.DeleteProject(ctx, "prj_x"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("重复删除应 NotFound，got %v", err)
	}
}

func TestChatRepo_ListSessionsByProjectAndMove(t *testing.T) {
	ctx := context.Background()
	db := newProjectTestDB(t)
	pr := NewSQLiteProjectRepository(db)
	cr := NewSQLiteChatRepository(db)

	_ = pr.CreateProject(ctx, types.ProjectRow{ID: "prj_a", Name: "A"})
	_ = cr.CreateSession(ctx, types.ChatSessionRow{ID: "a1", ProjectID: "prj_a"})
	_ = cr.CreateSession(ctx, types.ChatSessionRow{ID: "d1"})

	a, err := cr.ListProjectSessions(ctx, "prj_a", 50)
	if err != nil || len(a) != 1 || a[0].ID != "a1" || a[0].ProjectID != "prj_a" {
		t.Fatalf("ListProjectSessions(prj_a): %+v err=%v", a, err)
	}
	all, _ := cr.ListSessions(ctx, 50)
	if len(all) != 2 {
		t.Fatalf("ListSessions 应含全部会话，n=%d", len(all))
	}

	if err := cr.SetSessionProject(ctx, "d1", "prj_a"); err != nil {
		t.Fatalf("SetSessionProject: %v", err)
	}
	if a, _ := cr.ListProjectSessions(ctx, "prj_a", 50); len(a) != 2 {
		t.Fatalf("移动后 prj_a 应有 2 个会话，n=%d", len(a))
	}
	if err := cr.SetSessionProject(ctx, "d1", "no_such_project"); err == nil {
		t.Fatalf("移动到不存在项目应失败（外键）")
	}
	if err := cr.SetSessionProject(ctx, "ghost", "prj_a"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("移动不存在会话应 NotFound，got %v", err)
	}
}

// 已存在会话再次 CreateSession（INSERT OR IGNORE）不得改变其归属——
// 聊天请求携带不同 project_id 时以库内为准（ADR-0097 决策一）。
func TestChatRepo_CreateSessionDoesNotRehome(t *testing.T) {
	ctx := context.Background()
	db := newProjectTestDB(t)
	pr := NewSQLiteProjectRepository(db)
	cr := NewSQLiteChatRepository(db)

	_ = pr.CreateProject(ctx, types.ProjectRow{ID: "prj_a", Name: "A"})
	_ = pr.CreateProject(ctx, types.ProjectRow{ID: "prj_b", Name: "B"})
	_ = cr.CreateSession(ctx, types.ChatSessionRow{ID: "s", ProjectID: "prj_a"})
	_ = cr.CreateSession(ctx, types.ChatSessionRow{ID: "s", ProjectID: "prj_b"})

	got, _ := cr.GetSession(ctx, "s")
	if got == nil || got.ProjectID != "prj_a" {
		t.Fatalf("重复创建不应改变归属: %+v", got)
	}
}

// TestChatRepo_CreateSessionArchivedProject 归档项目拒收新会话，但其中既有会话照常可用；
// 不存在的项目返回 NotFound。
func TestChatRepo_CreateSessionArchivedProject(t *testing.T) {
	ctx := context.Background()
	db := newProjectTestDB(t)
	pr := NewSQLiteProjectRepository(db)
	cr := NewSQLiteChatRepository(db)

	if err := pr.CreateProject(ctx, types.ProjectRow{ID: "prj_arc", Name: "Arc"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s-old", ProjectID: "prj_arc"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := pr.UpdateProject(ctx, types.ProjectRow{ID: "prj_arc", Name: "Arc", Archived: true}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s-new", ProjectID: "prj_arc"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("归档项目新建会话应返回 InvalidInput，got %v", err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s-old", ProjectID: "prj_arc"}); err != nil {
		t.Fatalf("归档项目内既有会话应可继续：%v", err)
	}
	if err := cr.CreateSession(ctx, types.ChatSessionRow{ID: "s-x", ProjectID: "prj_nope"}); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的项目应返回 NotFound，got %v", err)
	}
}
