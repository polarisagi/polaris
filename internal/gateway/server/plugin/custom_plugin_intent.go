package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HandleCreatePluginFromIntent intent 模式实现：LLM 生成标准布局插件，随后与市场插件走同一
// 安装路径（InstallExtension 授权登记 → PluginInstaller 解析 / 子 MCP 独立授权 / 启动）。
// 由 HandleCreatePlugin 在确认 pluginCreator 非空且 intent 非空后调用；函数内部负责全部响应写入。
func (h *PluginHandler) HandleCreatePluginFromIntent(
	w http.ResponseWriter, r *http.Request,
	extID string, installReq protocol.ExtensionInstallRequest, intent string,
) {
	pluginDir, err := h.PluginCreator.GeneratePlugin(r.Context(), intent, 1 /* TrustLocal */)
	if err != nil {
		httputil.RespondError(w, "plugin_creator", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	pluginName := filepath.Base(pluginDir) // GeneratePlugin 以 result.Name 为目录名
	cfg, _ := json.Marshal(map[string]any{"intent": intent, "plugin_dir": pluginDir})
	installReq.Name = pluginName
	installReq.ExtType = "plugin"
	installReq.Config = string(cfg)
	installReq.LocalPath = pluginDir
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq); err != nil {
		httputil.RespondError(w, "plugin install", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	httputil.WriteJSONStatus(w, http.StatusCreated, map[string]any{
		"id": extID, "name": pluginName, "type": "plugin", "plugin_dir": pluginDir,
	})
}
