package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// HandleCreatePluginFromIntent intent 模式实现：LLM 生成 MCP 插件并注册为本地 MCP Server。
// 由 HandleCreatePlugin 在确认 pluginCreator 非空且 intent 非空后调用。
// 函数内部负责全部响应写入；调用方收到返回后直接 return。
func (h *PluginHandler) HandleCreatePluginFromIntent( //nolint:cyclop
	w http.ResponseWriter, r *http.Request,
	extID string, installReq protocol.ExtensionInstallRequest, intent string,
) {
	pluginDir, err := h.PluginCreator.GeneratePlugin(r.Context(), intent, 1 /* TrustLocal */)
	if err != nil {
		http.Error(w, "plugin_creator: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cfgPath, err := protocol.FindMCPConfig(pluginDir)
	if err != nil {
		http.Error(w, "plugin_creator: .mcp.json not found", http.StatusNotFound)
		return
	}
	mcpJSONRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		http.Error(w, "plugin_creator: read .mcp.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var mcpFileCfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpJSONRaw, &mcpFileCfg); err != nil {
		http.Error(w, "plugin_creator: parse .mcp.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// .mcp.json 中只有一个 server entry，取第一个
	var genCmd string
	var genArgs []string
	for _, srv := range mcpFileCfg.MCPServers {
		genCmd = srv.Command
		genArgs = srv.Args
		break
	}
	pluginName := filepath.Base(pluginDir) // GeneratePlugin 以 result.Name 为目录名

	now := time.Now().UTC().Format(time.RFC3339)
	mcpID := util.GenerateHumanReadableID("mcp", installReq.Name)
	genArgsBytes, _ := json.Marshal(genArgs)
	genConfigJSON, _ := json.Marshal(map[string]any{
		"transport":  "stdio",
		"command":    genCmd,
		"args":       genArgs,
		"plugin_dir": pluginDir,
	})

	// 写 mcp_servers（运行时表）
	if err := h.ExtRepo.UpsertMCPServer(r.Context(), apptypes.MCPServerRow{
		ID:        mcpID,
		Name:      pluginName,
		Transport: "stdio",
		Command:   genCmd,
		Args:      string(genArgsBytes),
		Env:       "{}",
		URL:       "",
		Enabled:   true,
		Timeout:   30,
		TrustTier: 1,
		CatalogID: "",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 写 extension_instances（安装 SSoT）
	installReq.Name = pluginName
	installReq.ExtType = "mcp" // 生成后本质是 MCP Server
	installReq.Config = string(genConfigJSON)
	installReq.RuntimeID = mcpID
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq); err != nil {
		_ = h.ExtRepo.DeleteMCPServer(r.Context(), mcpID)
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 异步启动 MCP Server
	if h.MCPMgr != nil {
		concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.plugin.start_mcp_server_from_intent", func(ctx context.Context) {
			_ = h.StartMCPServer(ctx, types.MCPServerConfig{
				ID:        mcpID,
				Name:      pluginName,
				Transport: "stdio",
				Command:   genCmd,
				Args:      genArgs,
				Timeout:   30,
				TrustTier: 1,
				Enabled:   true,
			})
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "mcp_id": mcpID, "name": pluginName,
		"type": "mcp", "plugin_dir": pluginDir,
	})
}
