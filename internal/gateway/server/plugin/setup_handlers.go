package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	reasonNotInstalled    = "not_installed"
	reasonNotEnabled      = "not_enabled"
	reasonDependencyUnmet = "dependency_unmet"
	reasonToolNotFound    = "tool_not_found"
	reasonToolNotVisible  = "tool_not_visible"
	reasonNoUIResource    = "no_ui_resource"
	reasonToolFailed      = "tool_failed"
	reasonPersistFailed   = "persist_failed"
	reasonLoadFailed      = "load_failed"
	reasonNotConfigured   = "not_configured"
	setupStatusTimeout    = 5 * time.Second
	setupExecutionTimeout = 60 * time.Second
	// statusTool 的逐项清单（ADR-0125 决策二）：插件给的是外部内容，按上限截断后才回给前端。
	maxSetupChecks     = 20
	maxSetupCheckField = 200
)

// AppToolExecutor 设置向导触发插件工具的唯一执行入口（实现：tool.InMemoryToolRegistry）。
// 与聊天回合同一执行体（PolicyGate / 审计 / 污点 / 幂等），principal 为 MCPApp；
// 不提供第二条执行路径，是为了让"不经 LLM"不等于"绕过安全门"（HE-3 / HE-7）。
type AppToolExecutor interface {
	ExecuteAppTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel, viewID, sessionID string) (*types.ToolResult, error)
}

// SetupStore 插件设置视图的落库（实现：chat.ChatPersistenceService）。设置视图不属于任何会话
// （ADR-0125），每个插件只保留最新一份：先删旧视图再落新视图。
type SetupStore interface {
	SaveAppView(ctx context.Context, v session.AppView) error
	DeletePluginAppViews(ctx context.Context, pluginID string) error
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

func (h *PluginHandler) loadInstalledPlugin(ctx context.Context, id string) (*installedPlugin, error) {
	var p installedPlugin
	var enabled int
	var manifest string
	err := h.DB.QueryRowContext(ctx,
		`SELECT id, name, display_name, enabled, manifest FROM plugins WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.DisplayName, &enabled, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		// 真实 DB 故障不能伪装成"未安装"：否则用户会被引导去重装一个其实存在的插件。
		return nil, apperr.Wrap(apperr.CodeInternal, "plugin setup: load plugin", err)
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
	case reasonNotInstalled, reasonToolNotFound:
		return http.StatusNotFound
	case reasonToolNotVisible:
		return http.StatusForbidden
	case reasonNotEnabled, reasonDependencyUnmet:
		return http.StatusConflict
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

// HandlePluginSetup POST /v1/plugins/{id}/setup：不经 LLM 直接触发插件的设置工具，把返回的
// MCP Apps 视图作为插件设置视图落库并返回，由插件详情页「设置」页就地挂载（ADR-0125）。
// 不新建会话、不写消息：此前每点一次设置都多出一个会话。
func (h *PluginHandler) HandlePluginSetup(w http.ResponseWriter, r *http.Request) {
	if h.DB == nil || h.AppTools == nil || h.SetupStore == nil {
		writeSetupError(w, reasonNotConfigured, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupExecutionTimeout)
	defer cancel()

	p, err := h.loadInstalledPlugin(ctx, r.PathValue("id"))
	if err != nil {
		writeSetupError(w, reasonLoadFailed, err)
		return
	}
	if p == nil {
		writeSetupError(w, reasonNotInstalled, nil)
		return
	}
	if p.Spec.SetupTool == "" {
		writeSetupError(w, reasonToolNotFound, nil)
		return
	}
	ref, reason := h.resolveSetupTool(ctx, p, p.Spec.SetupTool, h.listServers())
	if reason != "" {
		writeSetupError(w, reason, nil)
		return
	}

	// viewID 先于执行生成：审计里 app_view_id 要能对上之后落库的视图。
	viewID := types.NewToolUIViewID()
	input := []byte("{}")
	res, err := h.AppTools.ExecuteAppTool(ctx, ref.RegisteredName, input, types.TaintMedium, viewID, "")
	if err != nil || res == nil || !res.Success {
		writeSetupError(w, reasonToolFailed, err)
		return
	}
	ui := types.BuildToolUIRef(viewID, setupResourceURI(ref.Tool, res), ref.ServerID, ref.Tool.Name, input, res)
	if ui == nil {
		writeSetupError(w, reasonNoUIResource, nil)
		return
	}
	if err := h.SetupStore.DeletePluginAppViews(ctx, p.ID); err != nil {
		writeSetupError(w, reasonPersistFailed, err)
		return
	}
	if err := h.SetupStore.SaveAppView(ctx, session.AppView{PluginID: p.ID, ViewID: ui.ViewID, ServerID: ui.ServerID,
		ResourceURI: ui.ResourceURI, ToolName: ui.ToolName, ToolInput: ui.ToolInput, ToolResult: ui.ToolResult}); err != nil {
		writeSetupError(w, reasonPersistFailed, err)
		return
	}
	// 操作留痕：工具执行本身已由 Envelope 审计，这里补"哪个插件的设置视图被打开"。
	slog.Info("plugin.setup", "plugin", p.ID, "tool", ref.RegisteredName, "view_id", viewID)
	httputil.WriteJSON(w, map[string]any{"view": map[string]any{
		"view_id": ui.ViewID, "server_id": ui.ServerID, "resource_uri": ui.ResourceURI, "tool_name": ui.ToolName,
		"tool_input": ui.ToolInput, "tool_result": ui.ToolResult, "widget_state": json.RawMessage(`{}`),
	}})
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

// setupCheck statusTool 逐项清单的一项；Ok 为 nil 表示仅供参考、不影响就绪。
type setupCheck struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Ok     *bool  `json:"ok"`
	Detail string `json:"detail"`
}

// sanitizeSetupChecks 清单来自插件进程（外部内容）：类型不符的项丢弃，条数与字段长度截断。
func sanitizeSetupChecks(raw json.RawMessage) []setupCheck {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]setupCheck, 0, min(len(items), maxSetupChecks))
	for _, it := range items {
		if len(out) == maxSetupChecks {
			break
		}
		var c setupCheck
		if json.Unmarshal(it, &c) != nil || c.Label == "" {
			continue
		}
		c.Key, c.Label, c.Detail = clipRunes(c.Key), clipRunes(c.Label), clipRunes(c.Detail)
		out = append(out, c)
	}
	return out
}

func clipRunes(s string) string {
	if r := []rune(s); len(r) > maxSetupCheckField {
		return string(r[:maxSetupCheckField])
	}
	return s
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
		// 读错误与清单快照损坏都是"无法得出结论"，被轮询的接口不返回 5xx。
		slog.Warn("plugin setup-status: load plugin failed", "plugin", r.PathValue("id"), "err", err)
		unknown(reasonLoadFailed)
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
			Ready   *bool           `json:"ready"`
			Message string          `json:"message"`
			Checks  json.RawMessage `json:"checks"`
		} `json:"structuredContent"`
	}
	if json.Unmarshal(res.MCPRaw, &out) != nil || out.Structured.Ready == nil {
		unknown(reasonToolFailed)
		return
	}
	resp := map[string]any{"ready": *out.Structured.Ready, "message": clipRunes(out.Structured.Message)}
	if checks := sanitizeSetupChecks(out.Structured.Checks); len(checks) > 0 {
		resp["checks"] = checks
	}
	httputil.WriteJSON(w, resp)
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
