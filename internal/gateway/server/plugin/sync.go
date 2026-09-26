package plugin

import (
	"context"

	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// CatalogSyncer 市场目录同步（marketplace.CatalogSync 实现；只经两家与 MCP Registry 的标准目录格式）。
type CatalogSyncer interface {
	Sync(ctx context.Context, mp protocol.Marketplace, localOnly bool) ([]types.ExtCatalogRow, error)
}

// indexCatalogRows 异步触发 FTS + 向量预计算（不阻塞同步主流程）。
func (h *PluginHandler) indexCatalogRows(rows []types.ExtCatalogRow) {
	if h.EmbeddingIndexer == nil || len(rows) == 0 {
		return
	}
	entries := make([]CatalogEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, CatalogEntry{ID: r.ID, Name: r.Name, Description: r.Description})
	}
	// 使用后台 context（同步 ctx 可能已取消）
	concurrent.SafeGo(context.Background(), "gateway.plugin.index_catalog_entries", func(ctx context.Context) {
		h.EmbeddingIndexer.IndexEntries(ctx, entries)
	})
}

// SyncAllMarketplaces 后台静默同步所有可用市场并更新缓存
func (h *PluginHandler) SyncAllMarketplaces(ctx context.Context, localOnly bool) (int, error) {
	var mps []protocol.Marketplace
	rows, err := h.DB.QueryContext(ctx, "SELECT id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, created_at FROM plugin_marketplaces WHERE enabled=1")
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "Server.SyncAllMarketplaces", err)
	}
	for rows.Next() {
		var m protocol.Marketplace
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.Publisher, &m.RepoURL, &m.Description, &m.IsBuiltin, &m.TrustTier, &m.Enabled, &m.CreatedAt); err == nil {
			mps = append(mps, m)
		}
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "Server.SyncAllMarketplaces: rows", iterErr)
	}

	// 首先清理已经从活跃列表中移除的孤儿市场缓存
	activeIDs := make([]any, 0, len(mps))
	for _, mp := range mps {
		activeIDs = append(activeIDs, mp.ID)
	}
	if err := h.ExtRepo.DeleteOrphanCatalogEntries(ctx, activeIDs); err != nil {
		slog.Warn("plugin_sync: delete orphan catalog entries failed", "err", err)
	}

	if h.CatalogSync == nil {
		return 0, apperr.New(apperr.CodeInternal, "Server.SyncAllMarketplaces: catalog sync not configured")
	}
	syncedCount := 0
	for _, mp := range mps {
		rows, err := h.CatalogSync.Sync(ctx, mp, localOnly)
		if err != nil {
			// 单个市场失败不影响其余市场；原因留痕（HE-1）。
			slog.Warn("plugin_sync: marketplace sync failed", "marketplace", mp.ID, "err", err)
			continue
		}
		syncedCount += len(rows)
		h.indexCatalogRows(rows)
	}
	return syncedCount, nil
}

// HandleSyncMarketplaces 手动触发全量市场同步的 HTTP handler。
func (h *PluginHandler) HandleSyncMarketplaces(w http.ResponseWriter, r *http.Request) {
	localOnly := r.URL.Query().Get("local_only") == "true"
	slog.Info("polaris-server: manual sync marketplaces triggered", "local_only", localOnly)
	syncedCount, err := h.SyncAllMarketplaces(r.Context(), localOnly)
	if err != nil {
		slog.Error("polaris-server: manual sync marketplaces failed", "err", err)
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	slog.Info("polaris-server: manual sync marketplaces finished", "synced_count", syncedCount)
	httputil.WriteJSON(w, map[string]any{"status": "synced", "synced_count": syncedCount})
}
