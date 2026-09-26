package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// HandleCreateMCP 见 custom_mcp.go；HandleCreatePluginFromIntent 见 custom_plugin_intent.go（R7 拆分）。

// HandleCreateSkill 从用户给出的来源安装技能（agentskills 目录，如 Codex skill-installer 的
// GitHub 目录地址）。POST /v1/skills/create {"name","source"}
func (h *PluginHandler) HandleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	h.installFromSource(w, r, "skill", req.Name, req.Source)
}

// HandleCreatePlugin 用户创建插件，两种模式：
//   - source 模式：owner/repo[@ref]、GitHub 目录地址、https git / zip、npm:<pkg>、本机绝对路径（pluginspec.ParseSourceSpec）；
//   - intent 模式：由 PluginCreator 调用 LLM 生成标准布局插件后安装。
//
// POST /v1/plugins/create
func (h *PluginHandler) HandleCreatePlugin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Source string `json:"source"`
		Intent string `json:"intent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if req.Intent == "" || h.PluginCreator == nil {
		h.installFromSource(w, r, "plugin", req.Name, req.Source)
		return
	}
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	extID := util.GenerateHumanReadableID("ext", firstNonEmptyStr(req.Name, "plugin"))
	installReq := protocol.ExtensionInstallRequest{Principal: requestPrincipal(r), ExtensionID: extID, ExtType: "plugin",
		TrustTier: 1, Publisher: "user"}
	if err := h.InstallMgr.Authorize(r.Context(), installReq); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	h.HandleCreatePluginFromIntent(w, r, extID, installReq, req.Intent)
}

func requestPrincipal(r *http.Request) string {
	if p := authcontext.FromContext(r.Context()).UserID; p != "" {
		return p
	}
	return "user"
}

// installFromSource 校验来源 → 安装网关授权（需审批时走 HITL）→ 后台取回并安装。本机来源信任等级
// 按 TrustLocal（用户显式给出）。
func (h *PluginHandler) installFromSource(w http.ResponseWriter, r *http.Request, extType, name, spec string) {
	src, err := pluginspec.ParseSourceSpec(spec)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if h.InstallMgr == nil || h.Catalog == nil {
		http.Error(w, "installer not initialized", http.StatusServiceUnavailable)
		return
	}
	name = firstNonEmptyStr(name, sourceName(src))
	req := marketplace.SourceInstallRequest{ExtensionID: util.GenerateHumanReadableID("ext", name), Name: name,
		ExtType: extType, Principal: requestPrincipal(r), TrustTier: 1, Source: src}
	authReq := protocol.ExtensionInstallRequest{Principal: req.Principal, ExtensionID: req.ExtensionID, ExtType: extType,
		TrustTier: req.TrustTier, Publisher: "user"}
	if err := h.InstallMgr.Authorize(r.Context(), authReq); err != nil {
		if errors.Is(err, marketplace.ErrRequiresApproval) && h.HITLGateway != nil {
			h.installAfterApproval(r, req)
			httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]string{"status": "pending_approval", "id": req.ExtensionID})
			return
		}
		httputil.RespondError(w, "", err, http.StatusForbidden)
		return
	}
	h.runSourceInstall(r, req)
	httputil.WriteJSONStatus(w, http.StatusAccepted, map[string]any{"id": req.ExtensionID, "name": name, "type": extType, "status": "downloading"})
}

func (h *PluginHandler) runSourceInstall(r *http.Request, req marketplace.SourceInstallRequest) {
	concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.plugin.source_install", func(ctx context.Context) {
		if err := h.Catalog.InstallFromSource(ctx, req); err != nil {
			slog.Warn("plugin_custom: install from source failed", "id", req.ExtensionID, "err", err)
			h.updateExtensionInstanceError(ctx, req.ExtensionID, err.Error())
		}
	})
}

func (h *PluginHandler) installAfterApproval(r *http.Request, req marketplace.SourceInstallRequest) {
	bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
	concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_source_install", func(bgCtx context.Context) {
		defer cancel()
		resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{ID: req.ExtensionID, CheckpointType: "security_review",
			PromptText: "Approve installing " + req.ExtType + " " + req.Name + " from a user-provided source",
			Options:    []apptypes.HITLOption{{Key: "approve", Label: "Approve"}, {Key: "deny", Label: "Deny"}}})
		if err != nil || resp == nil || !resp.Approved {
			return
		}
		req.BypassAuth = true
		if err := h.Catalog.InstallFromSource(bgCtx, req); err != nil {
			slog.Warn("plugin_custom: install via HITL failed", "id", req.ExtensionID, "err", err)
		}
	})
}

// sourceName 未给名称时从来源推导（仓库名 / 子目录名 / 包名）。
func sourceName(src pluginspec.PluginSource) string {
	switch {
	case src.Path != "":
		return filepath.Base(src.Path)
	case src.Repo != "":
		return filepath.Base(src.Repo)
	case src.Package != "":
		return filepath.Base(src.Package)
	}
	return strings.TrimSuffix(filepath.Base(src.URL), ".git")
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
