package lifecycle

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	AppBound   = "bound"
	AppUnbound = "unbound"
)

// ResolveAppBindings Codex .app.json 的每个 alias 在本地连接器中按 id / 名称 / catalog_id 解析
// （ADR-0103 决策四）。app 是对已注册连接器的引用：命中即绑定，未命中（如 ChatGPT 平台专有
// connector_* ID）记为 unbound 由用户手动绑定——绝不据此生成 MCP 定义或进程。
// 用户的手动绑定在 connector_ref 未变且服务器仍存在时保留（升级重装不丢失）。
func ResolveAppBindings(ctx context.Context, extRepo protocol.ExtensionRepository, pluginID string, apps []pluginspec.AppBinding) error {
	servers, err := extRepo.ListMCPServers(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "ResolveAppBindings: servers", err)
	}
	previous, err := extRepo.ListPluginAppBindings(ctx, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "ResolveAppBindings: previous", err)
	}
	exists := map[string]bool{}
	for _, s := range servers {
		exists[s.ID] = true
	}
	kept := map[string]types.PluginAppBinding{}
	for _, b := range previous {
		if b.Status == AppBound && exists[b.BoundServerID] {
			kept[b.Alias] = b
		}
	}
	out := make([]types.PluginAppBinding, 0, len(apps))
	for _, app := range apps {
		b := types.PluginAppBinding{PluginID: pluginID, Alias: app.Alias, ConnectorRef: app.ConnectorID, Status: AppUnbound}
		if prev, ok := kept[app.Alias]; ok && prev.ConnectorRef == app.ConnectorID {
			b.BoundServerID, b.Status = prev.BoundServerID, AppBound
		} else if id, ok := matchConnector(servers, app.ConnectorID); ok {
			b.BoundServerID, b.Status = id, AppBound
		}
		out = append(out, b)
	}
	if err := extRepo.ReplacePluginAppBindings(ctx, pluginID, out); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "ResolveAppBindings: save", err)
	}
	return nil
}

func matchConnector(servers []types.MCPServerRow, ref string) (string, bool) {
	for _, s := range servers {
		if ref != "" && (s.ID == ref || s.Name == ref || s.CatalogID == ref) {
			return s.ID, true
		}
	}
	return "", false
}

// BindApp 用户手动把 alias 绑定到本地连接器（serverID 为空 = 解绑）。
func BindApp(ctx context.Context, extRepo protocol.ExtensionRepository, pluginID, alias, serverID string) (types.PluginAppBinding, error) {
	bindings, err := extRepo.ListPluginAppBindings(ctx, pluginID)
	if err != nil {
		return types.PluginAppBinding{}, apperr.Wrap(apperr.CodeOf(err), "BindApp", err)
	}
	idx := -1
	for i := range bindings {
		if bindings[i].Alias == alias {
			idx = i
		}
	}
	if idx < 0 {
		return types.PluginAppBinding{}, apperr.New(apperr.CodeNotFound, fmt.Sprintf("plugin %s has no app %q", pluginID, alias))
	}
	bindings[idx].BoundServerID, bindings[idx].Status = "", AppUnbound
	if serverID != "" {
		if srv, err := extRepo.GetMCPServer(ctx, serverID); err != nil || srv == nil {
			return types.PluginAppBinding{}, apperr.New(apperr.CodeNotFound, "connector not found: "+serverID)
		}
		bindings[idx].BoundServerID, bindings[idx].Status = serverID, AppBound
	}
	if err := extRepo.ReplacePluginAppBindings(ctx, pluginID, bindings); err != nil {
		return types.PluginAppBinding{}, apperr.Wrap(apperr.CodeOf(err), "BindApp: save", err)
	}
	return bindings[idx], nil
}
