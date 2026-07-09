package plugin

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
)

// GetInstalledCatalogIDs 返回所有已安装的 catalog_id 到 installed_version 的映射。
// SSoT：仅查 extension_instances，不再 UNION 多表。
func (h *PluginHandler) GetInstalledCatalogIDs(ctx context.Context) map[string]string {
	installed := map[string]string{}
	rows, err := h.DB.QueryContext(ctx,
		`SELECT catalog_id, config FROM extension_instances WHERE catalog_id != ''`)
	if err != nil {
		return installed
	}
	defer rows.Close()
	for rows.Next() {
		var cid, configStr string
		if rows.Scan(&cid, &configStr) == nil {
			version := ""
			var cfg map[string]any
			if json.Unmarshal([]byte(configStr), &cfg) == nil {
				if v, ok := cfg["version"].(string); ok {
					version = v
				}
			}
			installed[cid] = version
		}
	}
	return installed
}

// AppendCustomCatalogs 追加用户自建扩展（origin=user）到目录列表。
// 全走 extension_instances，不再散查 skills/plugins/apps 三表。
func (h *PluginHandler) AppendCustomCatalogs(ctx context.Context, result []protocol.RegistryEntry, _ map[string]string) []protocol.RegistryEntry {
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
	return result
}

// AppendCachedCatalogs 追加市场同步缓存条目，叠加安装状态。
func (h *PluginHandler) AppendCachedCatalogs(ctx context.Context, result []protocol.RegistryEntry, installed map[string]string) []protocol.RegistryEntry {
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
		if ver, ok := installed[entry.ID]; ok {
			entry.Installed = true
			entry.InstalledVersion = ver
		} else {
			entry.Installed = false
		}
		entry.MarketplaceSortOrder = sortOrder
		result = append(result, entry)
	}
	return result
}

// HandleListPluginCatalog 返回扩展目录列表（用户自建 + 市场缓存）。
// 排序规则：已安装优先 → 官方市场优先（SortOrder == 0）→ 名字字母序。
// 已安装的条目只出现一次（installed=true），不在未安装区重复展示。
// GET /v1/plugins/catalog
