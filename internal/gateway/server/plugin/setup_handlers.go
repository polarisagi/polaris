package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 设置向导不可用的机器码；前端按码做 i18n，后端不出展示文案。
const (
	reasonNotInstalled     = "not_installed"
	reasonNotEnabled       = "not_enabled"
	reasonDependencyUnmet  = "dependency_unmet"
	reasonToolNotFound     = "tool_not_found"
	reasonToolNotVisible   = "tool_not_visible"
	reasonSessionNotFound  = "session_not_found"
	reasonInvalidBody      = "invalid_body"
	reasonNoUIResource     = "no_ui_resource"
	reasonToolFailed       = "tool_failed"
	reasonPersistFailed    = "persist_failed"
	reasonNotConfigured    = "not_configured"
	setupStatusTimeout     = 5 * time.Second
	setupExecutionTimeout  = 60 * time.Second
	setupAssistantTemplate = "「%s」设置"
)

// AppToolExecutor 设置向导触发插件工具的唯一执行入口（实现：tool.InMemoryToolRegistry）。
// 与聊天回合同一执行体（PolicyGate / 审计 / 污点 / 幂等），principal 为 MCPApp；
// 不提供第二条执行路径，是为了让"不经 LLM"不等于"绕过安全门"（HE-3 / HE-7）。
type AppToolExecutor interface {
	ExecuteAppTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel, viewID, sessionID string) (*types.ToolResult, error)
}

// SetupStore 设置向导对会话/消息/视图落库的消费端接口（实现：chat.ChatPersistenceService）。
// 视图与消息的落库、关联复用 session.SaveToolUIView / LinkToolUIViews，与聊天回合同一实现。
type SetupStore interface {
	session.AppViewStore
	EnsureSession(ctx context.Context, sessionID string) error
	SessionExists(ctx context.Context, sessionID string) (bool, error)
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, durationMs int64) (int64, error)
	UpdateSessionTitle(ctx context.Context, sessionID, firstInput string) error
}

// setupInfo 插件列表里的 setup 块。
type setupInfo struct {
	Tool       string `json:"tool"`
	StatusTool string `json:"statusTool,omitempty"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
}

// installedPlugin 设置相关接口需要的插件行 + manifest 快照。
type installedPlugin struct {
	ID, Name, DisplayName string
	Enabled               bool
	Spec                  pluginspec.Plugin
}

func (p installedPlugin) title() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

func (h *PluginHandler) loadInstalledPlugin(ctx context.Context, id string) (*installedPlugin, error) {
	var p installedPlugin
	var enabled int
	var manifest string
	err := h.DB.QueryRowContext(ctx,
		`SELECT id, name, display_name, enabled, manifest FROM plugins WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.DisplayName, &enabled, &manifest)
	if err != nil {
		return nil, nil //nolint:nilerr // sql.ErrNoRows 与"其它读错误"对调用方都是不可用，统一当未安装
	}
	p.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(manifest), &p.Spec); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "plugin setup: corrupt manifest snapshot", err)
	}
	return &p, nil
}

// resolveSetupTool 校验插件当前是否具备执行 bare 工具的条件，返回解析结果或不可用的机器码。
// 顺序即用户的修复顺序：先启用，再满足依赖，再等工具就绪。
func (h *PluginHandler) resolveSetupTool(ctx context.Context, p *installedPlugin, bare string, servers []protocol.MCPServerInfo) (lifecycle.PluginToolRef, string) {
	if !p.Enabled {
		return lifecycle.PluginToolRef{}, reasonNotEnabled
	}
	if len(p.Spec.Dependencies) > 0 {
		// 管理器缺失时无法证明依赖满足，按不满足处理（fail-closed）。
		if h.Dependencies == nil || h.Dependencies.EnableBlocker(ctx, p.ID) != nil {
			return lifecycle.PluginToolRef{}, reasonDependencyUnmet
		}
	}
	ref, ok := lifecycle.ResolvePluginTool(servers, p.ID, bare)
	if !ok || !ref.Connected {
		return lifecycle.PluginToolRef{}, reasonToolNotFound
	}
	if !mcp.ToolUIVisibleTo(ref.Tool.UI, "app") && !mcp.ToolUIVisibleTo(ref.Tool.UI, "model") {
		return lifecycle.PluginToolRef{}, reasonToolNotVisible
	}
	return ref, ""
}

// setupInfoFor 供插件列表使用；未声明 setupTool 返回 nil（字段省略）。
func (h *PluginHandler) setupInfoFor(ctx context.Context, p *installedPlugin, servers []protocol.MCPServerInfo) *setupInfo {
	if p.Spec.SetupTool == "" {
		return nil
	}
	_, reason := h.resolveSetupTool(ctx, p, p.Spec.SetupTool, servers)
	return &setupInfo{Tool: p.Spec.SetupTool, StatusTool: p.Spec.StatusTool, Available: reason == "", Reason: reason}
}

func (h *PluginHandler) listServers() []protocol.MCPServerInfo {
	if h.MCPMgr == nil {
		return nil
	}
	return h.MCPMgr.ListServers()
}

// reasonStatus 机器码 → HTTP 状态。
func reasonStatus(reason string) int {
	switch reason {
	case reasonNotInstalled, reasonToolNotFound, reasonSessionNotFound:
		return http.StatusNotFound
	case reasonToolNotVisible:
		return http.StatusForbidden
	case reasonNotEnabled, reasonDependencyUnmet:
		return http.StatusConflict
	case reasonInvalidBody:
		return http.StatusBadRequest
	case reasonNotConfigured:
		return http.StatusServiceUnavailable
	case reasonNoUIResource, reasonToolFailed:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func writeSetupError(w http.ResponseWriter, reason string, err error) {
	if err != nil {
		slog.Warn("plugin setup failed", "reason", reason, "err", err)
	}
	httputil.WriteJSONStatus(w, reasonStatus(reason), map[string]string{"error": reason, "reason": reason})
}

// setupRequest 通过全部前置校验后的设置请求。
type setupRequest struct {
	plugin    *installedPlugin
	ref       lifecycle.PluginToolRef
	sessionID string
	isNew     bool
}

// prepareSetup 做执行前的全部校验；返回非空 reason 表示不可执行（err 仅用于日志/500）。
func (h *PluginHandler) prepareSetup(ctx context.Context, r *http.Request) (*setupRequest, string, error) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		return nil, reasonInvalidBody, apperr.Wrap(apperr.CodeInvalidInput, "plugin setup: decode body", err)
	}
	p, err := h.loadInstalledPlugin(ctx, r.PathValue("id"))
	if err != nil {
		return nil, reasonPersistFailed, err
	}
	if p == nil {
		return nil, reasonNotInstalled, nil
	}
	if p.Spec.SetupTool == "" {
		return nil, reasonToolNotFound, nil
	}
	ref, reason := h.resolveSetupTool(ctx, p, p.Spec.SetupTool, h.listServers())
	if reason != "" {
		return nil, reason, nil
	}
	req := &setupRequest{plugin: p, ref: ref, sessionID: body.SessionID, isNew: body.SessionID == ""}
	if req.isNew {
		req.sessionID = session.NewSessionID()
		return req, "", nil
	}
	ok, err := h.SetupStore.SessionExists(ctx, req.sessionID)
	if err != nil {
		return nil, reasonSessionNotFound, apperr.Wrap(apperr.CodeOf(err), "plugin setup: session lookup", err)
	}
	if !ok {
		return nil, reasonSessionNotFound, nil
	}
	return req, "", nil
}

// HandlePluginSetup POST /v1/plugins/{id}/setup：不经 LLM 直接触发插件的设置工具，
// 把返回的 MCP Apps 视图作为一条助手消息落进会话。
func (h *PluginHandler) HandlePluginSetup(w http.ResponseWriter, r *http.Request) {
	if h.DB == nil || h.AppTools == nil || h.SetupStore == nil {
		writeSetupError(w, reasonNotConfigured, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupExecutionTimeout)
	defer cancel()

	req, reason, err := h.prepareSetup(ctx, r)
	if reason != "" {
		writeSetupError(w, reason, err)
		return
	}

	// viewID 先于执行生成：审计里 app_view_id 要能对上之后落库的视图。
	viewID := types.NewToolUIViewID()
	input := []byte("{}")
	res, err := h.AppTools.ExecuteAppTool(ctx, req.ref.RegisteredName, input, types.TaintMedium, viewID, req.sessionID)
	if err != nil || res == nil || !res.Success {
		writeSetupError(w, reasonToolFailed, err)
		return
	}
	ui := types.BuildToolUIRef(viewID, setupResourceURI(req.ref.Tool, res), req.ref.ServerID, req.ref.Tool.Name, input, res)
	if ui == nil {
		writeSetupError(w, reasonNoUIResource, nil)
		return
	}

	messageID, err := h.persistSetupTurn(ctx, req, ui)
	if err != nil {
		writeSetupError(w, reasonPersistFailed, err)
		return
	}
	// 操作留痕：工具执行本身已由 Envelope 审计，这里补"谁在哪个会话触发了哪个插件的设置"。
	slog.Info("plugin.setup", "plugin", req.plugin.ID, "tool", req.ref.RegisteredName, "session_id", req.sessionID, "view_id", viewID)
	httputil.WriteJSON(w, map[string]any{"session_id": req.sessionID, "message_id": messageID, "view_id": viewID})
}

// setupResourceURI UI 资源来源：工具定义的 _meta.ui（MCP Apps 规范位置，聊天回合同此）优先，
// 其次工具结果自带的 _meta.ui，兼容在结果里动态指定视图的插件。
func setupResourceURI(tool protocol.MCPTool, res *types.ToolResult) string {
	if tool.UI != nil && tool.UI.ResourceURI != "" {
		return tool.UI.ResourceURI
	}
	var raw struct {
		Meta json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(res.MCPRaw, &raw) != nil {
		return ""
	}
	if ui := mcp.ParseToolUI(raw.Meta, tool.Name, ""); ui != nil {
		return ui.ResourceURI
	}
	return ""
}

// persistSetupTurn 与聊天回合同序：先落视图、再落助手消息、最后关联。
func (h *PluginHandler) persistSetupTurn(ctx context.Context, req *setupRequest, ui *types.ToolUIRef) (int64, error) {
	title := fmt.Sprintf(setupAssistantTemplate, req.plugin.title())
	if req.isNew {
		if err := h.SetupStore.EnsureSession(ctx, req.sessionID); err != nil {
			return 0, apperr.Wrap(apperr.CodeOf(err), "plugin setup: ensure session", err)
		}
		if err := h.SetupStore.UpdateSessionTitle(ctx, req.sessionID, title); err != nil {
			return 0, apperr.Wrap(apperr.CodeOf(err), "plugin setup: update title", err)
		}
	}
	if err := session.SaveToolUIView(ctx, h.SetupStore, req.sessionID, ui); err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "plugin setup: save view", err)
	}
	messageID, err := h.SetupStore.SaveMessage(ctx, req.sessionID, "assistant", title, "", "", 0)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "plugin setup: save message", err)
	}
	if messageID <= 0 {
		// 消息走了 outbox 兜底拿不到 ID：视图无法关联，历史里会丢失，这里没有"降级可接受"的余地。
		return 0, apperr.New(apperr.CodeInternal, "plugin setup: assistant message id unavailable")
	}
	if err := session.LinkToolUIViews(ctx, h.SetupStore, req.sessionID, []string{ui.ViewID}, messageID); err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "plugin setup: link view", err)
	}
	return messageID, nil
}

// HandlePluginSetupStatus GET /v1/plugins/{id}/setup-status：执行 statusTool 返回设置是否就绪。
// 任何无法得出结论的情形（未声明、插件未就绪、工具失败、超时）都返回 ready=null：
// 该接口被前端轮询，"未知"不应表现为 5xx。
func (h *PluginHandler) HandlePluginSetupStatus(w http.ResponseWriter, r *http.Request) {
	unknown := func(reason string) {
		httputil.WriteJSON(w, map[string]any{"ready": nil, "reason": reason})
	}
	if h.DB == nil || h.AppTools == nil {
		unknown(reasonNotConfigured)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupStatusTimeout)
	defer cancel()
	p, err := h.loadInstalledPlugin(ctx, r.PathValue("id"))
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if p == nil {
		writeSetupError(w, reasonNotInstalled, nil)
		return
	}
	if p.Spec.SetupTool == "" || p.Spec.StatusTool == "" {
		httputil.WriteJSON(w, map[string]any{"ready": nil})
		return
	}
	ref, reason := h.resolveSetupTool(ctx, p, p.Spec.StatusTool, h.listServers())
	if reason != "" {
		unknown(reason)
		return
	}
	// 状态查询没有对应视图与会话，viewID/sessionID 置空；执行体不依赖二者存在。
	res, err := h.AppTools.ExecuteAppTool(ctx, ref.RegisteredName, []byte("{}"), types.TaintMedium, "", "")
	if err != nil || res == nil || !res.Success {
		slog.Warn("plugin setup-status: status tool failed", "plugin", p.ID, "err", err)
		unknown(reasonToolFailed)
		return
	}
	var out struct {
		Structured struct {
			Ready   *bool  `json:"ready"`
			Message string `json:"message"`
		} `json:"structuredContent"`
	}
	if json.Unmarshal(res.MCPRaw, &out) != nil || out.Structured.Ready == nil {
		unknown(reasonToolFailed)
		return
	}
	httputil.WriteJSON(w, map[string]any{"ready": *out.Structured.Ready, "message": out.Structured.Message})
}

// fillManifestDerived 从清单快照派生列表字段；快照损坏只影响这两个附加字段，不拖垮整张列表。
func (h *PluginHandler) fillManifestDerived(ctx context.Context, resp *pluginResponse, servers []protocol.MCPServerInfo) {
	p := installedPlugin{ID: resp.ID, Name: resp.Name, DisplayName: resp.DisplayName, Enabled: resp.Enabled}
	if err := json.Unmarshal([]byte(resp.Manifest), &p.Spec); err != nil {
		return
	}
	if len(p.Spec.DefaultPrompts) > 0 {
		resp.DefaultPrompts = p.Spec.DefaultPrompts
	}
	resp.Setup = h.setupInfoFor(ctx, &p, servers)
}
