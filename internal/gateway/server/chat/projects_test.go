package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
)

// newProjectHandler 用真实 013_chat.sql 建库 + 真实 repo（手抄 DDL 会掩盖 schema 漂移）。
func newProjectHandler(t *testing.T) *ChatHandler {
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
	if _, err := db.Exec(`CREATE TABLE channels (id TEXT PRIMARY KEY, type TEXT)`); err != nil {
		t.Fatal(err)
	}
	return &ChatHandler{
		ProjectRepo:        repo.NewSQLiteProjectRepository(db),
		ChannelRepo:        repo.NewSQLiteChannelRepository(db),
		PersistenceService: &ChatPersistenceService{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db)},
	}
}

// asClient 构造带指定客户端类型的请求。
func asClient(method, path string, body any, ct authcontext.ClientType, authed bool, userID string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	ctx := authcontext.WithAuthContext(context.Background(), &authcontext.AuthContext{
		UserID: userID, ClientType: ct, Authenticated: authed,
	})
	return r.WithContext(ctx)
}

func local(method, path string, body any) *http.Request {
	return asClient(method, path, body, authcontext.ClientTypeLocalWebUI, true, "local")
}

func serve(h http.HandlerFunc, r *http.Request, pathVals map[string]string) *httptest.ResponseRecorder {
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func canonicalDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func decodeProject(t *testing.T, w *httptest.ResponseRecorder) projectDTO {
	t.Helper()
	var p projectDTO
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return p
}

func TestProjectsAPI_WriteSurfaceRestrictedToLocalTrusted(t *testing.T) {
	h := newProjectHandler(t)
	root := canonicalDir(t)

	// channel/API 客户端：读写全部 403（root_path/trusted/instructions 决定进 Prompt 的内容）。
	for name, hf := range map[string]http.HandlerFunc{
		"list":   h.HandleListProjects,
		"create": h.HandleCreateProject,
	} {
		r := asClient("POST", "/v1/projects", map[string]any{"name": "x", "root_path": root, "trusted": true},
			authcontext.ClientTypeAPI, false, "anonymous")
		if w := serve(hf, r, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s: 非本地客户端应 403，got %d", name, w.Code)
		}
	}
	if w := serve(h.HandleMoveSession, asClient("PUT", "/v1/sessions/s/project", map[string]any{"project_id": "default"},
		authcontext.ClientTypeAPI, false, "anonymous"), map[string]string{"sessionID": "s"}); w.Code != http.StatusForbidden {
		t.Errorf("move: 非本地客户端应 403，got %d", w.Code)
	}
	// admin（带 API key 的远程部署）允许。
	adm := asClient("POST", "/v1/projects", map[string]any{"name": "远程项目"}, authcontext.ClientTypeAPI, true, "admin")
	if w := serve(h.HandleCreateProject, adm, nil); w.Code != http.StatusCreated {
		t.Errorf("admin 应可创建，got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProjectsAPI_CreateNormalizesRootAndValidates(t *testing.T) {
	h := newProjectHandler(t)
	base := canonicalDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	// 软链输入必须落成规范路径。
	w := serve(h.HandleCreateProject, local("POST", "/v1/projects", map[string]any{"name": "P", "root_path": link, "trusted": true}), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	p := decodeProject(t, w)
	if p.RootPath != real || !p.Trusted {
		t.Fatalf("root_path 应存规范路径 %q，got %+v", real, p)
	}

	cases := map[string]map[string]any{
		"相对路径":         {"name": "x", "root_path": "relative/dir"},
		"不存在":          {"name": "x", "root_path": filepath.Join(base, "nope")},
		"文件系统根":        {"name": "x", "root_path": "/"},
		"空名":           {"name": "  "},
		"无目录却 trusted": {"name": "x", "trusted": true},
		"用户主目录":        {"name": "x", "root_path": "~"},
		"主目录的上级":       {"name": "x", "root_path": filepath.Dir(mustHome(t))},
	}
	for name, body := range cases {
		if w := serve(h.HandleCreateProject, local("POST", "/v1/projects", body), nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: 应 400，got %d %s", name, w.Code, w.Body.String())
		}
	}
	// 指向文件而非目录
	f := filepath.Join(base, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := serve(h.HandleCreateProject, local("POST", "/v1/projects", map[string]any{"name": "x", "root_path": f}), nil); w.Code != http.StatusBadRequest {
		t.Errorf("文件路径应 400，got %d", w.Code)
	}
}

func TestProjectsAPI_ChangingRootRevokesTrust(t *testing.T) {
	h := newProjectHandler(t)
	a, b := canonicalDir(t), canonicalDir(t)

	w := serve(h.HandleCreateProject, local("POST", "/v1/projects", map[string]any{"name": "P", "root_path": a, "trusted": true}), nil)
	p := decodeProject(t, w)

	w = serve(h.HandleUpdateProject, local("PUT", "/v1/projects/"+p.ID, map[string]any{"root_path": b}), map[string]string{"id": p.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if got := decodeProject(t, w); got.RootPath != b || got.Trusted {
		t.Fatalf("换目录后信任必须清零: %+v", got)
	}
}

func TestProjectsAPI_DefaultProjectProtected(t *testing.T) {
	h := newProjectHandler(t)
	if w := serve(h.HandleDeleteProject, local("DELETE", "/v1/projects/default", nil), map[string]string{"id": "default"}); w.Code != http.StatusBadRequest {
		t.Errorf("删默认项目应 400，got %d", w.Code)
	}
	root := canonicalDir(t)
	if w := serve(h.HandleUpdateProject, local("PUT", "/v1/projects/default", map[string]any{"root_path": root}), map[string]string{"id": "default"}); w.Code != http.StatusBadRequest {
		t.Errorf("默认项目绑目录应 400，got %d %s", w.Code, w.Body.String())
	}
	w := serve(h.HandleListProjects, local("GET", "/v1/projects", nil), nil)
	var out struct {
		Projects []projectDTO `json:"projects"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Projects) != 1 || !out.Projects[0].IsDefault {
		t.Fatalf("新库应恰有默认项目: %+v", out.Projects)
	}
}

func TestProjectsAPI_MoveSessionAndFilterList(t *testing.T) {
	h := newProjectHandler(t)
	ctx := context.Background()

	w := serve(h.HandleCreateProject, local("POST", "/v1/projects", map[string]any{"name": "P"}), nil)
	p := decodeProject(t, w)
	if err := h.PersistenceService.EnsureSessionInProject(ctx, "s1", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.PersistenceService.EnsureSession(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	// 不存在的项目 → 创建会话失败（外键），而不是悄悄落默认项目。
	if err := h.PersistenceService.EnsureSessionInProject(ctx, "s3", "prj_missing"); err == nil {
		t.Fatalf("会话绑定不存在的项目应失败")
	}

	list := func(q string) []map[string]any {
		w := serve(h.HandleListSessions, local("GET", "/v1/sessions"+q, nil), nil)
		var out struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("list: %d %v %s", w.Code, err, w.Body.String())
		}
		return out.Sessions
	}
	if got := list("?project_id=" + p.ID); len(got) != 1 || got[0]["id"] != "s1" || got[0]["project_id"] != p.ID {
		t.Fatalf("按项目过滤: %+v", got)
	}
	if got := list(""); len(got) != 2 {
		t.Fatalf("缺省应返回全部会话，got %d", len(got))
	}

	w = serve(h.HandleMoveSession, local("PUT", "/v1/sessions/s2/project", map[string]any{"project_id": p.ID}), map[string]string{"sessionID": "s2"})
	if w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if got := list("?project_id=" + p.ID); len(got) != 2 {
		t.Fatalf("移动后项目应有 2 个会话，got %d", len(got))
	}
	w = serve(h.HandleMoveSession, local("PUT", "/v1/sessions/s2/project", map[string]any{"project_id": "prj_missing"}), map[string]string{"sessionID": "s2"})
	if w.Code != http.StatusNotFound {
		t.Errorf("移动到不存在项目应 404，got %d", w.Code)
	}

	// 删除项目：会话迁回默认项目而非被级联删除。
	if w := serve(h.HandleDeleteProject, local("DELETE", "/v1/projects/"+p.ID, nil), map[string]string{"id": p.ID}); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if got := list("?project_id=default"); len(got) != 2 {
		t.Fatalf("删项目后会话应迁回默认项目，got %d", len(got))
	}
}

func TestProjectsAPI_GetProject(t *testing.T) {
	h := newProjectHandler(t)
	w := serve(h.HandleCreateProject, local("POST", "/v1/projects", map[string]any{"name": "G", "instructions": "读我"}), nil)
	p := decodeProject(t, w)

	w = serve(h.HandleGetProject, local("GET", "/v1/projects/"+p.ID, nil), map[string]string{"id": p.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if got := decodeProject(t, w); got.ID != p.ID || got.Instructions != "读我" {
		t.Fatalf("get 内容不符: %+v", got)
	}
	if w := serve(h.HandleGetProject, local("GET", "/v1/projects/nope", nil), map[string]string{"id": "nope"}); w.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404，got %d", w.Code)
	}
	// 非本地可信客户端读取同样 403（root_path 本身即隐私信息）。
	if w := serve(h.HandleGetProject, asClient("GET", "/v1/projects/"+p.ID, nil, authcontext.ClientTypeAPI, false, "anonymous"),
		map[string]string{"id": p.ID}); w.Code != http.StatusForbidden {
		t.Fatalf("非本地客户端读应 403，got %d", w.Code)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无 HOME: %v", err)
	}
	return home
}
