package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// hookSourceView hooks.json 来源的审阅视图：展示完整处理器定义（审阅的就是将被执行的内容）。
type hookSourceView struct {
	Key        string                     `json:"key"`
	Scope      hook.Scope                 `json:"scope"`
	PluginID   string                     `json:"plugin_id,omitempty"`
	PluginName string                     `json:"plugin_name,omitempty"`
	Digest     string                     `json:"digest"`
	Trusted    bool                       `json:"trusted"`
	Events     map[string]json.RawMessage `json:"events"`
}

// HandleListHooks 列出全部 hooks 来源及信任状态。
// GET /v1/hooks
func (h *PluginHandler) HandleListHooks(w http.ResponseWriter, r *http.Request) {
	if h.HookRunner == nil {
		http.Error(w, "hook engine not initialized", http.StatusServiceUnavailable)
		return
	}
	if err := h.HookRunner.Registry().Reload(r.Context()); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	var out []hookSourceView
	for _, src := range h.HookRunner.Registry().Sources() {
		events := map[string]json.RawMessage{}
		for ev, groups := range src.Config {
			raw, _ := json.Marshal(groups)
			events[string(ev)] = raw
		}
		out = append(out, hookSourceView{Key: src.Key, Scope: src.Scope, PluginID: src.PluginID, PluginName: src.PluginName,
			Digest: src.Digest, Trusted: src.Trusted, Events: events})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	httputil.WriteJSON(w, map[string]any{"sources": out})
}

// HandleTrustHook 按定义哈希信任一个来源（安装 ≠ 信任，ADR-0103 决策六）。digest 必须等于当前
// 定义的哈希：审阅后定义若已变更，旧审阅不得覆盖新内容。
// POST /v1/hooks/trust  body: {"key": "...", "digest": "..."}
func (h *PluginHandler) HandleTrustHook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key    string `json:"key"`
		Digest string `json:"digest"`
	}
	if !h.authorizeHookAdmin(w, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" || body.Digest == "" {
		http.Error(w, "key and digest required", http.StatusBadRequest)
		return
	}
	src, ok := h.findHookSource(r.Context(), body.Key)
	if !ok {
		http.Error(w, "hook source not found", http.StatusNotFound)
		return
	}
	if src.Scope == hook.ScopeUser {
		http.Error(w, "user-level hooks are always trusted", http.StatusBadRequest)
		return
	}
	if src.Digest != body.Digest {
		http.Error(w, "hook definition changed since review; reload and review again", http.StatusConflict)
		return
	}
	if err := h.ExtRepo.SaveHookTrust(r.Context(), body.Key, body.Digest); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	h.reloadHooks(r.Context())
	httputil.WriteJSON(w, map[string]any{"status": "trusted", "key": body.Key})
}

// HandleRevokeHookTrust 撤销信任。
// DELETE /v1/hooks/trust?key=...
func (h *PluginHandler) HandleRevokeHookTrust(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeHookAdmin(w, r) {
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	if err := h.ExtRepo.DeleteHookTrust(r.Context(), key); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	h.reloadHooks(r.Context())
	httputil.WriteJSON(w, map[string]any{"status": "revoked", "key": key})
}

func (h *PluginHandler) authorizeHookAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.HookRunner == nil || h.InstallMgr == nil {
		http.Error(w, "hook engine not initialized", http.StatusServiceUnavailable)
		return false
	}
	principal := authcontext.FromContext(r.Context()).UserID
	if principal == "" {
		principal = "user"
	}
	if err := h.InstallMgr.AuthorizeAction(r.Context(), principal, "plugin:manage", nil); err != nil {
		httputil.RespondError(w, "", err, http.StatusForbidden)
		return false
	}
	return true
}

func (h *PluginHandler) findHookSource(ctx context.Context, key string) (hook.Source, bool) {
	h.reloadHooks(ctx)
	for _, src := range h.HookRunner.Registry().Sources() {
		if src.Key == key {
			return src, true
		}
	}
	return hook.Source{}, false
}

// reloadHooks 插件启停 / 信任变更后刷新 hook 来源快照。
func (h *PluginHandler) reloadHooks(ctx context.Context) {
	if h.HookRunner == nil {
		return
	}
	if err := h.HookRunner.Registry().Reload(ctx); err != nil {
		slog.Warn("plugin: reload hook sources failed", "err", err)
	}
}
