package chat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 项目（Project）HTTP 处理器（ADR-0097）──────────────────────────────────
//
// 写入面收窄（ADR-0097 决策二）：创建/修改/删除项目、改会话归属仅限本地可信客户端
// 或 admin。channel / Webhook 客户端无权设置 root_path、trusted、instructions——
// 这三者决定"哪些文件内容以什么信任级别进入 Prompt"，等价于提示词写入面。

const (
	maxProjectNameRunes    = 100
	maxProjectInstructions = 8 * 1024 // 字节
)

// projectDTO 对外表示。root_path 仅本地可信客户端 / admin 可见之外的调用方不会走到这里
// （读接口同样受 requireProjectAccess 约束——目录路径本身即隐私信息）。
type projectDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RootPath     string `json:"root_path"`
	Instructions string `json:"instructions"`
	Trusted      bool   `json:"trusted"`
	Archived     bool   `json:"archived"`
	IsDefault    bool   `json:"is_default"`
	SessionCount int    `json:"session_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func toProjectDTO(p types.ProjectRow) projectDTO {
	return projectDTO{
		ID: p.ID, Name: p.Name, RootPath: p.RootPath, Instructions: p.Instructions,
		Trusted: p.Trusted, Archived: p.Archived, IsDefault: p.ID == protorepo.DefaultProjectID,
		SessionCount: p.SessionCount, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// requireProjectAccess 项目读写口径与 server_handlers_hitl.go 中断接口一致：
// 本地可信客户端或 admin。返回 false 时已写入 403。
func (h *ChatHandler) requireProjectAccess(w http.ResponseWriter, r *http.Request) bool {
	a := authcontext.FromContext(r.Context())
	isAdmin := a.Authenticated && (a.UserID == "admin" || a.UserID == "system")
	if !isAdmin && !a.ClientType.IsLocalTrusted() {
		httputil.RespondError(w, "forbidden: project management requires a local trusted client",
			apperr.New(apperr.CodeForbidden, "project management requires a local trusted client"), http.StatusForbidden)
		return false
	}
	return true
}

func (h *ChatHandler) projectsUnavailable(w http.ResponseWriter) bool {
	if h.ProjectRepo == nil {
		httputil.RespondError(w, "projects not configured", apperr.New(apperr.CodeInternal, "project repository not configured"), http.StatusInternalServerError)
		return true
	}
	return false
}

// respondProjectErr 把 apperr Code 映射为 HTTP 状态。
func respondProjectErr(w http.ResponseWriter, err error) {
	code := apperr.CodeOf(err)
	status := apperr.HTTPStatus(code)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	// 4xx 的 Message 是面向用户的校验提示（如"root_path 不存在或无法访问"），原样回传给
	// UI；5xx 不外泄内部细节，走默认文案。
	msg := ""
	var ae *apperr.Error
	if status < http.StatusInternalServerError && errors.As(err, &ae) {
		msg = ae.Message
	}
	httputil.RespondError(w, msg, err, status)
}

func newProjectID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("prj_%x", os.Getpid())
	}
	return "prj_" + hex.EncodeToString(b)
}

// normalizeProjectRoot 把用户输入的目录规范化为规范路径（Abs + EvalSymlinks）。
//
// 为什么存规范路径：信任判定是路径前缀比较；若存用户输入的原始路径，"信任 /a/proj，
// 而 /a/proj 是指向别处的软链"即可绕过（见 workspace_context.go ListForProject）。
// 空串合法，表示无目录项目。
func normalizeProjectRoot(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInvalidInput, "无法解析 ~", err)
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~"))
	}
	if !filepath.IsAbs(raw) {
		return "", apperr.New(apperr.CodeInvalidInput, "root_path 必须是绝对路径")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "root_path 不存在或无法访问", err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", apperr.New(apperr.CodeInvalidInput, "root_path 必须是目录")
	}
	// 文件系统根目录作为项目根等于"信任整台机器"，拒绝。
	if resolved == filepath.VolumeName(resolved)+string(os.PathSeparator) {
		return "", apperr.New(apperr.CodeInvalidInput, "root_path 不能是文件系统根目录")
	}
	// 项目目录同时是该项目会话的工具文件访问根（ADR-0097 决策五）：不得位于敏感目录内，
	// 也不得是用户主目录或敏感目录的祖先。与工具侧 guard.ScopedPaths 同一判据。
	if err := guard.CheckScopeRoot(resolved); err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "root_path 不能是用户主目录、敏感目录或它们的上级目录", err)
	}
	return resolved, nil
}

func validateProjectFields(name, instructions string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return apperr.New(apperr.CodeInvalidInput, "name 不能为空")
	}
	if utf8.RuneCountInString(name) > maxProjectNameRunes {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("name 最长 %d 字符", maxProjectNameRunes))
	}
	if len(instructions) > maxProjectInstructions {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("instructions 最长 %d 字节", maxProjectInstructions))
	}
	return nil
}

// GET /v1/projects[?include_archived=true]
func (h *ChatHandler) HandleListProjects(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) || h.projectsUnavailable(w) {
		return
	}
	rows, err := h.ProjectRepo.ListProjects(r.Context(), r.URL.Query().Get("include_archived") == "true")
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	list := make([]projectDTO, 0, len(rows))
	for _, p := range rows {
		list = append(list, toProjectDTO(p))
	}
	httputil.WriteJSON(w, map[string]any{"projects": list})
}

// GET /v1/projects/{id}
func (h *ChatHandler) HandleGetProject(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) || h.projectsUnavailable(w) {
		return
	}
	p, err := h.ProjectRepo.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	httputil.WriteJSON(w, toProjectDTO(*p))
}

// POST /v1/projects  {name, root_path?, instructions?, trusted?}
func (h *ChatHandler) HandleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) || h.projectsUnavailable(w) {
		return
	}
	var req struct {
		Name         string `json:"name"`
		RootPath     string `json:"root_path"`
		Instructions string `json:"instructions"`
		Trusted      bool   `json:"trusted"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err), http.StatusBadRequest)
		return
	}
	if err := validateProjectFields(req.Name, req.Instructions); err != nil {
		respondProjectErr(w, err)
		return
	}
	root, err := normalizeProjectRoot(req.RootPath)
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	if req.Trusted && root == "" {
		respondProjectErr(w, apperr.New(apperr.CodeInvalidInput, "trusted 仅对绑定了目录的项目有意义"))
		return
	}
	p := types.ProjectRow{
		ID: newProjectID(), Name: strings.TrimSpace(req.Name), RootPath: root,
		Instructions: req.Instructions, Trusted: req.Trusted,
	}
	if err := h.ProjectRepo.CreateProject(r.Context(), p); err != nil {
		respondProjectErr(w, err)
		return
	}
	if p.Trusted {
		slog.Info("project: created with trusted workspace", "project", p.ID, "root", p.RootPath,
			"actor", authcontext.FromContext(r.Context()).UserID)
	}
	created, err := h.ProjectRepo.GetProject(r.Context(), p.ID)
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	httputil.WriteJSON(w, toProjectDTO(*created))
}

// projectPatch 是 PUT /v1/projects/{id} 的载荷，字段均可选，仅更新出现的字段。
type projectPatch struct {
	Name         *string `json:"name"`
	RootPath     *string `json:"root_path"`
	Instructions *string `json:"instructions"`
	Trusted      *bool   `json:"trusted"`
	Archived     *bool   `json:"archived"`
}

// apply 把补丁叠加到当前行并校验。独立成函数是为了把信任语义集中在一处：
// 换目录 = 换信任对象，旧授予不得延续；但请求显式带 trusted 时以请求为准。
func (p projectPatch) apply(cur types.ProjectRow) (types.ProjectRow, error) {
	next := cur
	if p.Name != nil {
		next.Name = strings.TrimSpace(*p.Name)
	}
	if p.Instructions != nil {
		next.Instructions = *p.Instructions
	}
	if p.RootPath != nil {
		root, err := normalizeProjectRoot(*p.RootPath)
		if err != nil {
			return next, err
		}
		if root != cur.RootPath {
			next.Trusted = false
		}
		next.RootPath = root
	}
	if p.Trusted != nil {
		next.Trusted = *p.Trusted
	}
	if p.Archived != nil {
		next.Archived = *p.Archived
	}
	if err := validateProjectFields(next.Name, next.Instructions); err != nil {
		return next, err
	}
	if next.Trusted && next.RootPath == "" {
		return next, apperr.New(apperr.CodeInvalidInput, "trusted 仅对绑定了目录的项目有意义")
	}
	return next, nil
}

// PUT /v1/projects/{id}
func (h *ChatHandler) HandleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) || h.projectsUnavailable(w) {
		return
	}
	var req projectPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err), http.StatusBadRequest)
		return
	}
	cur, err := h.ProjectRepo.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	next, err := req.apply(*cur)
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	if err := h.ProjectRepo.UpdateProject(r.Context(), next); err != nil {
		respondProjectErr(w, err)
		return
	}
	if next.Trusted && !cur.Trusted {
		slog.Info("project: workspace trust granted", "project", next.ID, "root", next.RootPath,
			"actor", authcontext.FromContext(r.Context()).UserID)
	}
	updated, err := h.ProjectRepo.GetProject(r.Context(), next.ID)
	if err != nil {
		respondProjectErr(w, err)
		return
	}
	httputil.WriteJSON(w, toProjectDTO(*updated))
}

// DELETE /v1/projects/{id}  其会话迁回默认项目，不级联删除。
func (h *ChatHandler) HandleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) || h.projectsUnavailable(w) {
		return
	}
	if err := h.ProjectRepo.DeleteProject(r.Context(), r.PathValue("id")); err != nil {
		respondProjectErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /v1/sessions/{sessionID}/project  {project_id}
func (h *ChatHandler) HandleMoveSession(w http.ResponseWriter, r *http.Request) {
	if !h.requireProjectAccess(w, r) {
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", apperr.Wrap(apperr.CodeInvalidInput, "invalid request body", err), http.StatusBadRequest)
		return
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	if !session.SessionIDPattern.MatchString(req.ProjectID) {
		respondProjectErr(w, apperr.New(apperr.CodeInvalidInput, "invalid project_id"))
		return
	}
	sessionID := r.PathValue("sessionID")
	if err := h.PersistenceService.ChatRepo.SetSessionProject(r.Context(), sessionID, req.ProjectID); err != nil {
		respondProjectErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"session_id": sessionID, "project_id": req.ProjectID})
}
