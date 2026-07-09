package plugin

import (
	"context"

	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// SkillFrontmatter 是 SKILL.md frontmatter 的完整解析结果（agentskills.io 开放标准字段）。
type SkillFrontmatter struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description"`
	Version         string   `yaml:"version"`
	Tags            []string `yaml:"tags"`
	ExecMode        string   `yaml:"exec_mode"`        // "tool"（默认）| "ambient"
	AmbientPriority string   `yaml:"ambient_priority"` // "always" | "auto"（默认）| "index_only"
	RiskLevel       string   `yaml:"risk_level"`       // "low" | "medium" | "high"
	Sandbox         string   `yaml:"sandbox"`          // "L1" | "L2" | "L3"
	Capability      string   `yaml:"capability"`       // e.g. "read-write"
}

// pullOrClone 通过 downloader.GitCloneOrPull 同步单个市场仓库。
// 在中国大陆网络下自动走 ghproxy 加速。
func pullOrClone(repoURL, mpDir string) (available bool, updated bool) {
	return downloader.GitCloneOrPull(context.Background(), nil, repoURL, mpDir)
}

// syncMarketplace 同步单个市场
func (h *PluginHandler) syncMarketplace(ctx context.Context, mp protocol.Marketplace, tmpDir string, localOnly bool) int {
	if mp.RepoURL == "" {
		return 0
	}

	safeID := strings.ReplaceAll(mp.ID, "/", "_")
	mpDir := filepath.Join(tmpDir, safeID)

	var available, updated bool
	if localOnly {
		if _, err := os.Stat(mpDir); err == nil {
			available = true
			updated = true
		}
	} else {
		available, updated = pullOrClone(mp.RepoURL, mpDir)
	}

	if !available {
		return 0
	}
	if !updated {
		// 仓库无新变化；若 catalog 已有条目（正常情况）则跳过，节省解析开销。
		// 若 catalog 为空（如 DB 重建），仍需重新写库，否则插件列表永久为空。
		var count int
		_ = h.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM extension_catalog WHERE marketplace_id=?", mp.ID).Scan(&count)
		if count > 0 {
			return 0
		}
	}

	b, err := os.ReadFile(filepath.Join(mpDir, "catalog.json"))
	if err != nil {
		entries, scanErr := discoverMarketplaceEntries(mpDir, mp)
		if scanErr == nil && len(entries) > 0 {
			b, _ = json.Marshal(entries)
		} else {
			return 0
		}
	}

	var entries []protocol.RegistryEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return 0
	}

	return h.insertMarketplaceEntries(ctx, mp, mpDir, entries)
}

// insertMarketplaceEntries 将 entries 插入数据库，减少外层函数的圈复杂度。
func (h *PluginHandler) insertMarketplaceEntries(ctx context.Context, mp protocol.Marketplace, mpDir string, entries []protocol.RegistryEntry) int {
	defaultVersion := downloader.GitShortHash(mpDir)
	rows := make([]types.ExtCatalogRow, 0, len(entries))

	for i := range entries {
		e := &entries[i]
		e.Publisher = mp.Publisher
		e.TrustTier = mp.TrustTier
		if e.Version == "" && defaultVersion != "" {
			e.Version = defaultVersion
		}
		payload, _ := json.Marshal(e)

		rows = append(rows, types.ExtCatalogRow{
			ID:            e.ID,
			MarketplaceID: mp.ID,
			Type:          e.Type,
			Name:          e.Name,
			Description:   e.Description,
			Publisher:     mp.Publisher,
			TrustTier:     mp.TrustTier,
			URL:           e.URL,
			Payload:       string(payload),
		})
	}

	syncedCount, _ := h.ExtRepo.ReplaceMarketplaceCatalog(ctx, mp.ID, rows)

	// 异步触发 FTS + 向量预计算（不阻塞同步主流程）
	if h.EmbeddingIndexer != nil && syncedCount > 0 {
		catalogEntries := make([]CatalogEntry, 0, len(rows))
		for _, r := range rows {
			catalogEntries = append(catalogEntries, CatalogEntry{
				ID:          r.ID,
				Name:        r.Name,
				Description: r.Description,
			})
		}
		// 使用后台 context（同步 ctx 可能已取消）
		concurrent.SafeGo(context.Background(), "gateway.plugin.index_catalog_entries", func(ctx context.Context) {
			h.EmbeddingIndexer.IndexEntries(ctx, catalogEntries)
		})
	}

	return syncedCount
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
	rows.Close()

	tmpDir := filepath.Join(h.DataDir, "tmp", "marketplaces")
	_ = os.MkdirAll(tmpDir, 0755)

	// 首先清理已经从活跃列表中移除的孤儿市场缓存
	activeIDs := make([]any, 0, len(mps))
	for _, mp := range mps {
		activeIDs = append(activeIDs, mp.ID)
	}
	_ = h.ExtRepo.DeleteOrphanCatalogEntries(ctx, activeIDs)

	syncedCount := 0
	for _, mp := range mps {
		syncedCount += h.syncMarketplace(ctx, mp, tmpDir, localOnly)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("polaris-server: manual sync marketplaces finished", "synced_count", syncedCount)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "synced", "synced_count": syncedCount})
}

type PackageJSON struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Homepage     string            `json:"homepage"`
	Dependencies map[string]string `json:"dependencies"`
}
