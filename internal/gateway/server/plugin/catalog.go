package plugin

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
)

// installedInstance 目录条目叠加用的已安装实例信息。
type installedInstance struct {
	ID      string // extension_instances.id，原地升级接口 POST /v1/plugins/{id}/upgrade 用它定位
	Version string // installed_version；ADR-0124 之前的安装为空
}

// GetInstalledCatalogIDs 返回所有已安装的 catalog_id 到实例信息的映射。
// SSoT：仅查 extension_instances.installed_version——此前读 config.version，而目录安装把 config
// 写成 "{}"，已装版本恒为空，插件页永远看不到「升级」。
func (h *PluginHandler) GetInstalledCatalogIDs(ctx context.Context) map[string]installedInstance {
	installed := map[string]installedInstance{}
	rows, err := h.DB.QueryContext(ctx,
		`SELECT catalog_id, id, installed_version FROM extension_instances WHERE catalog_id != ''`)
	if err != nil {
		return installed
	}
	defer rows.Close()
	for rows.Next() {
		var cid string
		var inst installedInstance
		if rows.Scan(&cid, &inst.ID, &inst.Version) == nil {
			installed[cid] = inst
		}
	}
	if err := rows.Err(); err != nil {
		// 迭代中途出错会静默少行，表现为已装插件显示成未安装；记录后返回已读部分。
		slog.WarnContext(ctx, "plugin catalog: list installed instances incomplete", "err", err)
	}
	return installed
}

// AppendCustomCatalogs 追加用户自建扩展（origin=user）到目录列表。
// 全走 extension_instances，不再散查 skills/plugins/apps 三表。
func (h *PluginHandler) AppendCustomCatalogs(ctx context.Context, result []protocol.RegistryEntry, _ map[string]installedInstance) []protocol.RegistryEntry {
	rows, err := h.DB.QueryContext(ctx,
		`SELECT id, ext_type, name, publisher, trust_tier, config
		 FROM extension_instances
		 WHERE origin = 'user' AND status = 'installed'`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var e protocol.RegistryEntry
		var configJSON string
		if err := rows.Scan(&e.ID, &e.Type, &e.Name, &e.Publisher, &e.TrustTier, &configJSON); err != nil {
			continue
		}
		e.Installed = true
		// 从 config JSON 提取 URL（app）/ command（mcp）等展示字段，容错忽略
		var cfg map[string]any
		if json.Unmarshal([]byte(configJSON), &cfg) == nil {
			if v, ok := cfg["url"].(string); ok {
				e.URL = v
			}
			if v, ok := cfg["command"].(string); ok {
				e.Command = v
			}
		}
		e.MarketplaceSortOrder = 1000 // 用户自定义的扩展放到同类状态的最后
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "plugin catalog: list custom extensions incomplete", "err", err)
	}
	return result
}

// AppendCachedCatalogs 追加市场同步缓存条目，叠加安装状态。
func (h *PluginHandler) AppendCachedCatalogs(ctx context.Context, result []protocol.RegistryEntry, installed map[string]installedInstance) []protocol.RegistryEntry {
	rows, err := h.DB.QueryContext(ctx, `
		SELECT c.payload, COALESCE(m.sort_order, 999) 
		FROM extension_catalog c 
		LEFT JOIN plugin_marketplaces m ON c.marketplace_id = m.id`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var payload string
		var sortOrder int
		if err := rows.Scan(&payload, &sortOrder); err != nil {
			continue
		}
		var entry protocol.RegistryEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		if inst, ok := installed[entry.ID]; ok {
			entry.Installed = true
			entry.InstanceID = inst.ID
			entry.InstalledVersion = inst.Version
			// 已装版本未知（历史安装）也提示升级：原地升级会回写版本，之后即恢复正常比较。
			entry.UpdateAvailable = entry.Version != "" && inst.Version != entry.Version
		} else {
			entry.Installed = false
		}
		entry.MarketplaceSortOrder = sortOrder
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "plugin catalog: list cached catalog incomplete", "err", err)
	}
	return result
}
