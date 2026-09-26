package marketplace

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CatalogSync 市场目录同步（ADR-0103 决策七），只经标准格式：
//   - plugin 市场：`.claude-plugin/marketplace.json` / `.agents/plugins/marketplace.json`（pluginspec 解析）；
//   - skill 市场：agentskills.io 仓库（SKILL.md 目录，pluginspec 解析）；
//   - mcp 市场：MCP Registry（server.json）。
//
// 删除了此前的启发式爬取与私有 catalog.json——两家与 MCP 生态都已有标准目录格式。
type CatalogSync struct {
	extRepo  protocol.ExtensionRepository
	registry *MCPMarketplaceClient
	http     network.SafeHTTPClient
	cacheDir string
}

func NewCatalogSync(extRepo protocol.ExtensionRepository, registry *MCPMarketplaceClient, httpClient network.SafeHTTPClient, cacheDir string) *CatalogSync {
	return &CatalogSync{extRepo: extRepo, registry: registry, http: httpClient, cacheDir: cacheDir}
}

// MarketplaceDir 市场缓存目录（市场 ID 可含 "/"）。
func (s *CatalogSync) MarketplaceDir(mpID string) string {
	return filepath.Join(s.cacheDir, strings.ReplaceAll(mpID, "/", "_"))
}

// Sync 同步单个市场并整体替换其目录条目；返回写入的行（供调用方做检索索引）。
func (s *CatalogSync) Sync(ctx context.Context, mp protocol.Marketplace, localOnly bool) ([]types.ExtCatalogRow, error) {
	entries, err := s.entries(ctx, mp, localOnly)
	if err != nil {
		return nil, err
	}
	rows := make([]types.ExtCatalogRow, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		e.Publisher = firstNonEmpty(e.Publisher, mp.Publisher)
		e.TrustTier = mp.TrustTier
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "CatalogSync: marshal "+e.ID, err)
		}
		rows = append(rows, types.ExtCatalogRow{ID: e.ID, MarketplaceID: mp.ID, Type: e.Type, Name: e.Name,
			Description: e.Description, Publisher: e.Publisher, TrustTier: mp.TrustTier, URL: e.URL, Version: e.Version,
			Payload: string(payload)})
	}
	if _, err := s.extRepo.ReplaceMarketplaceCatalog(ctx, mp.ID, rows); err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "CatalogSync: replace catalog "+mp.ID, err)
	}
	return rows, nil
}

func (s *CatalogSync) entries(ctx context.Context, mp protocol.Marketplace, localOnly bool) ([]protocol.RegistryEntry, error) {
	if mp.Type == "mcp" {
		if localOnly || s.registry == nil {
			return nil, apperr.New(apperr.CodeUnimplemented, "CatalogSync: MCP registry unavailable")
		}
		entries, err := s.registry.ListRegistry(ctx, mp.RepoURL)
		for i := range entries {
			entries[i].ID = mp.ID + "/" + entries[i].Name
		}
		return entries, err
	}
	root, fromURL, err := s.materialize(ctx, mp, localOnly)
	if err != nil {
		return nil, err
	}
	var entries []protocol.RegistryEntry
	if mp.Type == "skill" {
		entries = skillEntries(mp, root)
	} else if entries, err = pluginEntries(mp, root, fromURL); err != nil {
		return nil, err
	}
	// 条目无版本时以市场仓库提交作为版本（升级检测用）。
	if hash := downloader.GitShortHash(root); hash != "" {
		for i := range entries {
			entries[i].Version = firstNonEmpty(entries[i].Version, hash)
		}
	}
	return entries, nil
}

// materialize 取得市场根目录：git 仓库（clone / pull）、本地目录，或直链 marketplace.json。
func (s *CatalogSync) materialize(ctx context.Context, mp protocol.Marketplace, localOnly bool) (string, bool, error) {
	u := mp.RepoURL
	if filepath.IsAbs(u) {
		return u, false, nil
	}
	dir := s.MarketplaceDir(mp.ID)
	if strings.HasSuffix(strings.ToLower(u), ".json") {
		return dir, true, s.fetchMarketplaceJSON(ctx, u, dir, localOnly)
	}
	if localOnly {
		if _, err := os.Stat(dir); err != nil {
			return "", false, apperr.New(apperr.CodeNotFound, "CatalogSync: no local copy of "+mp.ID)
		}
		return dir, false, nil
	}
	validator := func(url string) error { return network.ValidateGitURL(ctx, url) }
	if repo, ref := splitRef(u); ref != "" {
		// 固定 ref 的市场每次按该 ref 重新浅拉取（分支会前进，标签可被移动）。
		if err := os.RemoveAll(dir); err != nil {
			return "", false, apperr.Wrap(apperr.CodeInternal, "CatalogSync: reset "+dir, err)
		}
		if err := downloader.GitFetchRevision(ctx, validator, repo, ref, "", dir); err != nil {
			return "", false, apperr.Wrap(apperr.CodeOf(err), "CatalogSync: fetch "+u, err)
		}
		return dir, false, nil
	}
	available, _ := downloader.GitCloneOrPull(ctx, nil, validator, u, dir)
	if !available {
		return "", false, apperr.New(apperr.CodeNetworkUnavailable, "CatalogSync: fetch "+u)
	}
	return dir, false, nil
}

func (s *CatalogSync) fetchMarketplaceJSON(ctx context.Context, u, dir string, localOnly bool) error {
	dest := filepath.Join(dir, ".claude-plugin", "marketplace.json")
	if localOnly {
		return nil
	}
	fetcher := NewSourceFetcher(s.http, s.cacheDir)
	tmp, _, err := fetcher.download(ctx, u, nil, "marketplace-*.json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CatalogSync: mkdir", err)
	}
	return os.Rename(tmp, dest) //nolint:wrapcheck
}

// pluginEntries 市场清单 → 目录条目。NOT_AVAILABLE（Codex）不列出；直链市场无法解析相对路径来源
// （只有 marketplace.json 本身，Claude 规则），此类条目跳过；保留名市场整体拒绝。
func pluginEntries(mp protocol.Marketplace, root string, fromURL bool) ([]protocol.RegistryEntry, error) {
	m, err := pluginspec.GetMarketplace(root)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "CatalogSync: "+mp.ID, err)
	}
	if pluginspec.ReservedMarketplaceName(m.Name, strings.HasPrefix(mp.RepoURL, "https://github.com/anthropics/")) {
		return nil, apperr.New(apperr.CodeForbidden, "CatalogSync: marketplace name "+m.Name+" is reserved")
	}
	for _, d := range m.Diagnostics {
		slog.Warn("marketplace: catalog diagnostic", "marketplace", mp.ID, "diag", d.String())
	}
	out := make([]protocol.RegistryEntry, 0, len(m.Plugins))
	for _, e := range m.Plugins {
		if (e.Policy != nil && e.Policy.Installation == "NOT_AVAILABLE") || (fromURL && e.Source.Type == pluginspec.SourceRelative) {
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "CatalogSync: marshal entry", err)
		}
		out = append(out, protocol.RegistryEntry{ID: mp.ID + "/" + e.Name, Type: "plugin", Name: e.Name,
			DisplayName: e.DisplayName, Description: e.Description, Version: e.Version, Tags: append(e.Tags, e.Keywords...),
			Homepage: e.Homepage, MarketplaceName: m.Name, Entry: raw, AllowCrossDeps: m.AllowCrossMarketplaceDeps})
	}
	return out, nil
}

// skillEntries agentskills 仓库：每个含 SKILL.md 的目录一个条目（Codex skill-installer 同一粒度）。
func skillEntries(mp protocol.Marketplace, root string) []protocol.RegistryEntry {
	var out []protocol.RegistryEntry
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // 单个不可读目录不影响其余技能
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		dir := filepath.Dir(path)
		sk, _ := pluginspec.ParseSkillDir(dir)
		if sk == nil {
			return nil
		}
		rel, _ := filepath.Rel(root, dir)
		out = append(out, protocol.RegistryEntry{ID: mp.ID + "/" + filepath.ToSlash(rel), Type: "skill", Name: sk.Name,
			Description: sk.Description, SourceDir: dir})
		return nil
	})
	if walkErr != nil {
		slog.Warn("marketplace: skill scan failed", "marketplace", mp.ID, "err", walkErr)
	}
	return out
}
