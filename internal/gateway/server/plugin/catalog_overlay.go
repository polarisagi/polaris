package plugin

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// CatalogOverlaySyncer 目录覆盖层同步（marketplace.OverlaySync 实现，ADR-0123）。
type CatalogOverlaySyncer interface {
	Sync(ctx context.Context) (int, error)
}

// loadCatalogOverlay 读取覆盖层行与词表。任何失败都只降级为「无覆盖层」：覆盖层只影响展示，
// 不能因为它让目录接口整体失败。
func (h *PluginHandler) loadCatalogOverlay(ctx context.Context) (map[string]types.ExtCatalogOverlayRow, json.RawMessage) {
	if h.ExtRepo == nil {
		return nil, nil
	}
	rows, err := h.ExtRepo.ListCatalogOverlay(ctx)
	if err != nil {
		slog.Warn("plugin_catalog: load overlay failed", "err", err)
		return nil, nil
	}
	tax, err := h.ExtRepo.GetCatalogOverlayMeta(ctx, marketplace.OverlayMetaTaxonomy)
	if err != nil || tax == "" || !json.Valid([]byte(tax)) {
		return rows, nil
	}
	return rows, json.RawMessage(tax)
}

// applyCatalogOverlay 把覆盖层叠加到目录条目（原地修改）。过期判断必须用条目原始描述，
// 所以先算指纹、再用 description_en 补空描述。
func applyCatalogOverlay(entries []protocol.RegistryEntry, overlay map[string]types.ExtCatalogOverlayRow) {
	if len(overlay) == 0 {
		return
	}
	for i := range entries {
		e := &entries[i]
		o, ok := overlay[e.ID]
		if !ok {
			continue
		}
		e.I18nStale = marketplace.OverlaySourceHash(e.Name, e.Description) != o.SrcHash
		if e.Description == "" && o.DescriptionEn != "" {
			e.Description = o.DescriptionEn
		}
		e.NameZh, e.SummaryZh, e.DescriptionZh = o.NameZh, o.SummaryZh, o.DescriptionZh
		e.Topics = decodeFacet(o.Topics)
		e.Departments = decodeFacet(o.Departments)
		e.Industries = decodeFacet(o.Industries)
		e.Life = decodeFacet(o.Life)
		e.Featured = o.Featured
	}
}

// decodeFacet 解析 JSON 字符串数组；坏数据视为空（落库前已由 SanitizeOverlay 清洗）。
func decodeFacet(s string) []string {
	var out []string
	if s == "" || json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// syncCatalogOverlay 市场同步之后拉取覆盖层；失败只留痕，不影响同步结果（HE-1）。
func (h *PluginHandler) syncCatalogOverlay(ctx context.Context) {
	if h.CatalogOverlay == nil {
		return
	}
	n, err := h.CatalogOverlay.Sync(ctx)
	if err != nil {
		slog.Warn("plugin_sync: catalog overlay sync failed", "err", err)
		return
	}
	slog.Info("plugin_sync: catalog overlay synced", "entries", n, "not_modified", n == 0)
}
