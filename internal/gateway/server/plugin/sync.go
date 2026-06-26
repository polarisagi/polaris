package plugin

import (
	"context"

	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
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

// parseFrontmatter 从 SKILL.md 内容中提取 YAML frontmatter 并解析到 SkillFrontmatter。
// 找不到 frontmatter 时返回零值结构体，不返回错误（降级处理）。
func parseFrontmatter(content string) SkillFrontmatter {
	lines := strings.Split(content, "\n")
	firstDash, secondDash := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if firstDash == -1 {
				firstDash = i
			} else {
				secondDash = i
				break
			}
		}
	}
	var fm SkillFrontmatter
	if firstDash != -1 && secondDash != -1 && secondDash > firstDash+1 {
		_ = yaml.Unmarshal([]byte(strings.Join(lines[firstDash+1:secondDash], "\n")), &fm)
	}
	if fm.ExecMode == "" {
		fm.ExecMode = "tool"
	}
	if fm.AmbientPriority == "" {
		fm.AmbientPriority = "auto"
	}
	if fm.RiskLevel == "" {
		fm.RiskLevel = "medium"
	}
	if fm.Version == "" {
		fm.Version = "1.0.0"
	}
	return fm
}

// sandboxLevel 将 SKILL.md 的 "L1"/"L2"/"L3" 映射到 skills.sandbox INTEGER。
func sandboxLevel(s string) int {
	switch strings.ToUpper(s) {
	case "L3":
		return 3
	case "L2":
		return 2
	default:
		return 1
	}
}

// parseSkillMD 从 SKILL.md 中解析 Frontmatter 元数据（保持向后兼容）。
func parseSkillMD(content string) (string, string, []string, string) {
	fm := parseFrontmatter(content)
	return fm.Name, fm.Description, fm.Tags, fm.ExecMode
}

// formatName 将连字符分隔的目录名格式化为人类可读的名称。
// 这是为了在 Frontmatter 中没有指定 name 字段时提供一个优雅的后备方案。
func formatName(s string) string {
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// parseSkillEntry 解析技能市场的 SKILL.md 文件并返回 protocol.RegistryEntry。
func parseSkillEntry(path string, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	relDir, err := filepath.Rel(mpDir, filepath.Dir(path))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseSkillEntry", err)
	}
	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseSkillEntry", err)
	}

	name, desc, tags, _ := parseSkillMD(string(contentBytes))
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
		name = formatName(name)
	}
	if desc == "" {
		desc = "Auto-detected skill in " + relPath
	}

	url := mp.RepoURL
	if strings.Contains(url, "github.com") {
		url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
	}

	return &protocol.RegistryEntry{
		ID:          mp.ID + "/" + relPath,
		Publisher:   mp.Publisher,
		Type:        "skill",
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: desc,
		URL:         url,
		Tags:        tags,
		Timeout:     60,
	}, nil
}

// parsePluginEntry 解析插件市场的 plugin.json 文件并返回 protocol.RegistryEntry。
func parsePluginEntry(path string, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	relDir, err := filepath.Rel(mpDir, filepath.Dir(path))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePluginEntry", err)
	}

	// 如果 plugin.json 在 .claude-plugin / .polaris-plugin / .codex-plugin 目录下，其上级目录才是插件主目录
	if b := filepath.Base(relDir); b == ".claude-plugin" || b == ".polaris-plugin" || b == ".codex-plugin" {
		relDir = filepath.Dir(relDir)
	}

	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePluginEntry", err)
	}

	var pJSON protocol.PluginJSON
	var name, desc string
	var tags []string
	var displayName, shortDesc, icon string
	if err := json.Unmarshal(contentBytes, &pJSON); err == nil {
		name = pJSON.Name
		desc = pJSON.Description
		tags = pJSON.Keywords
		if pJSON.Interface != nil {
			displayName = pJSON.Interface.DisplayName
			shortDesc = pJSON.Interface.ShortDescription
			icon = pJSON.Interface.IconSmall
		}
	}

	if name == "" {
		name = filepath.Base(relDir)
		name = formatName(name)
	}
	if desc == "" {
		desc = "Auto-detected plugin in " + relPath
	}

	url := mp.RepoURL
	if strings.Contains(url, "github.com") {
		url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
	}

	return &protocol.RegistryEntry{
		ID:               mp.ID + "/" + relPath,
		Publisher:        mp.Publisher,
		Type:             "plugin",
		TrustTier:        mp.TrustTier,
		Name:             name,
		Description:      desc,
		URL:              url,
		Tags:             tags,
		Homepage:         pJSON.Homepage,
		DisplayName:      displayName,
		ShortDescription: shortDesc,
		Icon:             icon,
		Timeout:          60,
	}, nil
}

// parseMCPEntry 解析市场的 mcp.json 文件并返回 []protocol.RegistryEntry。
func parseMCPEntry(path string, mpDir string, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) {
	relDir, err := filepath.Rel(mpDir, filepath.Dir(path))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseMCPEntry", err)
	}
	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseMCPEntry", err)
	}

	var mcpConfig protocol.MCPConfig
	if err := json.Unmarshal(contentBytes, &mcpConfig); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseMCPEntry", err)
	}

	// 兼容扁平格式
	if len(mcpConfig.MCPServers) == 0 {
		var flat map[string]protocol.MCPServerDef
		if err := json.Unmarshal(contentBytes, &flat); err == nil {
			filtered := make(map[string]protocol.MCPServerDef)
			for k, v := range flat {
				if k != "mcpServers" && (v.Command != "" || v.URL != "") {
					filtered[k] = v
				}
			}
			mcpConfig.MCPServers = filtered
		}
	}

	entries := make([]protocol.RegistryEntry, 0, len(mcpConfig.MCPServers))
	for srvName, srvDef := range mcpConfig.MCPServers {
		url := mp.RepoURL
		if strings.Contains(url, "github.com") {
			url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
		}

		name := srvName
		if name == "" {
			name = filepath.Base(relDir)
			name = formatName(name)
		}

		transport := "stdio"
		if srvDef.URL != "" {
			transport = "sse"
		}

		entries = append(entries, protocol.RegistryEntry{
			ID:          mp.ID + "/" + relPath + "/" + srvName,
			Publisher:   mp.Publisher,
			Type:        "mcp",
			TrustTier:   mp.TrustTier,
			Name:        name,
			Description: srvName + " MCP Server",
			URL:         url,
			Timeout:     60,
			Transport:   transport,
			Command:     srvDef.Command,
			Args:        srvDef.Args,
			Env:         srvDef.Env,
		})
	}

	return entries, nil
}

// isPluginBundleRoot 探测目录是否包含明确的插件边界配置文件
func isPluginBundleRoot(dir string) (string, string) {
	manifests := []struct {
		relPath string
		typ     string
	}{
		{"plugin.json", "plugin.json"},
		{".claude-plugin/plugin.json", "plugin.json"},
		{".polaris-plugin/plugin.json", "plugin.json"},
		{".codex-plugin/plugin.json", "plugin.json"},
		{"ai-plugin.json", "ai-plugin.json"},
		{"plugin.toml", "plugin.toml"},
		{".claude-plugin/plugin.toml", "plugin.toml"},
		{"skills.yaml", "skills.yaml"},
		{"agent-manifest.yaml", "agent-manifest.yaml"},
		{"mcp.json", "mcp.json"},
		{".mcp.json", "mcp.json"},
		{"package.json", "package.json"},
		{"pyproject.toml", "pyproject.toml"},
	}

	for _, m := range manifests {
		p := filepath.Join(dir, m.relPath)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, m.typ
		}
	}
	return "", ""
}

// parseBundleManifest 根据清单类型解析插件包。
func parseBundleManifest( //nolint:gocyclo
	manifestPath, manifestType, mpDir string, mp protocol.Marketplace) []protocol.RegistryEntry {
	var entries []protocol.RegistryEntry
	switch manifestType {
	case "plugin.json":
		if entry, err := parsePluginEntry(manifestPath, mpDir, mp); err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	case "ai-plugin.json":
		if entry, err := parseAIPluginEntry(manifestPath, mpDir, mp); err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	case "plugin.toml":
		if entry, err := parsePluginTOMLEntry(manifestPath, mpDir, mp); err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	case "skills.yaml", "agent-manifest.yaml":
		if newEntries, err := parseGoogleSkillsEntry(manifestPath, mpDir, mp); err == nil {
			entries = append(entries, newEntries...)
		}
	case "mcp.json":
		if newEntries, err := parseMCPEntry(manifestPath, mpDir, mp); err == nil {
			entries = append(entries, newEntries...)
		}
	case "package.json":
		if entry, err := parsePackageJSONEntry(manifestPath, mpDir, mp); err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	case "pyproject.toml":
		if entry, err := parsePyProjectTOMLEntry(manifestPath, mpDir, mp); err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// discoverMarketplaceEntries 递归遍历市场目录，自动发现所有的插件和技能。
// 引入 Bundle Root Detection，遇到完整插件包则不再拆解其内部的子技能。
func discoverMarketplaceEntries(mpDir string, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) { //nolint:gocyclo
	var entries []protocol.RegistryEntry

	err := filepath.Walk(mpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "discoverMarketplaceEntries", err)
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}

			// 如果当前目录是一个插件包（如 discord 目录包含了 .claude-plugin/plugin.json），则整体作为一个插件条目，不再继续深入遍历其 skills/
			manifestPath, manifestType := isPluginBundleRoot(path)
			if manifestPath != "" {
				entries = append(entries, parseBundleManifest(manifestPath, manifestType, mpDir, mp)...)
				// 核心：跳过进入该包内部（如 external_plugins/discord/skills），防止内部碎片能力被提取到全局市场
				return filepath.SkipDir
			}
			return nil
		}

		// 只有在未被标记为 Plugin 包的独立散装目录中，才会提取单独的组件
		if info.Name() == "SKILL.md" {
			if entry, err := parseSkillEntry(path, mpDir, mp); err == nil && entry != nil {
				entries = append(entries, *entry)
			}
		} else if info.Name() == "mcp.json" {
			if newEntries, err := parseMCPEntry(path, mpDir, mp); err == nil {
				entries = append(entries, newEntries...)
			}
		}

		return nil
	})

	if err != nil {
		return entries, apperr.Wrap(apperr.CodeInternal, "discoverMarketplaceEntries", err)
	}
	return entries, nil
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
		go func() {
			// 使用后台 context（同步 ctx 可能已取消）
			h.EmbeddingIndexer.IndexEntries(context.Background(), catalogEntries)
		}()
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

// parseAIPluginEntry 解析 OpenAI ai-plugin.json 格式。
// api.type=="mcp" 时映射为 mcp 条目；其余映射为 app（URL 直连）。
func parseAIPluginEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseAIPluginEntry", err)
	}
	var p protocol.AIPluginJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseAIPluginEntry", err)
	}
	relDir, _ := filepath.Rel(mpDir, filepath.Dir(path))
	relPath := filepath.ToSlash(relDir)

	name := p.NameForHuman
	if name == "" {
		name = p.NameForModel
	}
	if name == "" {
		name = filepath.Base(relDir)
		name = formatName(name)
	}
	desc := p.DescriptionForHuman
	if desc == "" {
		desc = p.DescriptionForModel
	}

	extType := "app"
	command := ""
	if strings.EqualFold(p.API.Type, "mcp") {
		extType = "mcp"
		command = p.API.URL
	}

	url := p.API.URL
	if strings.Contains(mp.RepoURL, "github.com") && url == "" {
		url = strings.TrimSuffix(mp.RepoURL, "/") + "/tree/main/" + relPath
	}

	return &protocol.RegistryEntry{
		ID:          mp.ID + "/" + relPath,
		Publisher:   mp.Publisher,
		Type:        extType,
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: desc,
		URL:         url,
		Homepage:    p.LegalInfoURL,
		Command:     command,
		Timeout:     60,
	}, nil
}

var errSkipEntry = apperr.New(apperr.CodeInternal, "skip entry")

// parsePluginTOMLEntry 解析 Anthropic plugin.toml（根目录或 .claude-plugin/ 下）。
func parsePluginTOMLEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePluginTOMLEntry", err)
	}
	var p protocol.AnthropicPluginTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePluginTOMLEntry", err)
	}
	if p.Plugin.Name == "" && p.MCP.Command == "" {
		return nil, errSkipEntry
	}

	relDir, _ := filepath.Rel(mpDir, filepath.Dir(path))
	// plugin.toml 在 .claude-plugin/ 子目录时，ID 取其上级目录
	if filepath.Base(relDir) == ".claude-plugin" {
		relDir = filepath.Dir(relDir)
	}
	relPath := filepath.ToSlash(relDir)

	name := p.Plugin.Name
	if name == "" {
		name = filepath.Base(relDir)
		name = formatName(name)
	}

	extType := "mcp"
	if p.MCP.Command == "" {
		extType = "plugin"
	}

	url := mp.RepoURL
	if strings.Contains(url, "github.com") {
		url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
	}

	return &protocol.RegistryEntry{
		ID:          mp.ID + "/" + relPath,
		Publisher:   mp.Publisher,
		Type:        extType,
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: p.Plugin.Description,
		URL:         url,
		Command:     p.MCP.Command,
		Args:        p.MCP.Args,
		Env:         p.MCP.Env,
		Timeout:     60,
	}, nil
}

// parseGoogleSkillsEntry 解析 Google skills.yaml / agent-manifest.yaml 格式。
// 顶层有 command 时映射为 mcp；否则映射为 skill。多 skills 列表逐条展开。
func parseGoogleSkillsEntry(path, mpDir string, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseGoogleSkillsEntry", err)
	}
	var g protocol.GoogleSkillsYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseGoogleSkillsEntry", err)
	}

	relDir, _ := filepath.Rel(mpDir, filepath.Dir(path))
	relPath := filepath.ToSlash(relDir)
	baseID := mp.ID + "/" + relPath

	baseURL := mp.RepoURL
	if strings.Contains(baseURL, "github.com") {
		baseURL = strings.TrimSuffix(baseURL, "/") + "/tree/main/" + relPath
	}

	// 单条目
	if len(g.Skills) == 0 {
		extType := "skill"
		if g.Command != "" {
			extType = "mcp"
		}
		name := g.Name
		if name == "" {
			name = filepath.Base(relDir)
			name = formatName(name)
		}
		return []protocol.RegistryEntry{{
			ID:          baseID,
			Publisher:   mp.Publisher,
			Type:        extType,
			TrustTier:   mp.TrustTier,
			Name:        name,
			Description: g.Description,
			URL:         baseURL,
			Command:     g.Command,
			Args:        g.Args,
			Timeout:     60,
		}}, nil
	}

	// 多技能列表
	entries := make([]protocol.RegistryEntry, 0, len(g.Skills))
	for i, h := range g.Skills {
		if h.Name == "" {
			continue
		}
		extType := "skill"
		if h.Command != "" {
			extType = "mcp"
		}
		entries = append(entries, protocol.RegistryEntry{
			ID:          fmt.Sprintf("%s/skill_%d", baseID, i),
			Publisher:   mp.Publisher,
			Type:        extType,
			TrustTier:   mp.TrustTier,
			Name:        h.Name,
			Description: h.Description,
			URL:         baseURL,
			Command:     h.Command,
			Args:        h.Args,
			Timeout:     60,
		})
	}
	return entries, nil
}

// handleSyncMarketplaces 刷新/同步市场
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

func parsePackageJSONEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePackageJSONEntry", err)
	}
	var p PackageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePackageJSONEntry", err)
	}

	isMCP := strings.Contains(p.Name, "mcp")

	for k := range p.Dependencies {
		if strings.Contains(k, "modelcontextprotocol") || k == "mcp" {
			isMCP = true
			break
		}
	}
	if !isMCP {
		return nil, errSkipEntry
	}

	relDir, _ := filepath.Rel(mpDir, filepath.Dir(path))
	relPath := filepath.ToSlash(relDir)

	name := p.Name
	if name == "" {
		name = filepath.Base(relDir)
		name = formatName(name)
	}

	url := mp.RepoURL
	if strings.Contains(url, "github.com") {
		url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
	}

	cmd := "npx"
	args := []string{"-y", p.Name + "@latest"}

	return &protocol.RegistryEntry{
		ID:          mp.ID + "/" + relPath,
		Publisher:   mp.Publisher,
		Type:        "mcp",
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: p.Description,
		URL:         url,
		Homepage:    p.Homepage,
		Command:     cmd,
		Args:        args,
		Timeout:     60,
	}, nil
}

type PyProjectTOML struct {
	Project struct {
		Name         string   `toml:"name"`
		Description  string   `toml:"description"`
		Version      string   `toml:"version"`
		Dependencies []string `toml:"dependencies"`
	} `toml:"project"`
}

func parsePyProjectTOMLEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePyProjectTOMLEntry", err)
	}
	var p PyProjectTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parsePyProjectTOMLEntry", err)
	}

	isMCP := strings.Contains(p.Project.Name, "mcp")

	for _, dep := range p.Project.Dependencies {
		if strings.Contains(dep, "mcp") {
			isMCP = true
			break
		}
	}
	if !isMCP {
		return nil, errSkipEntry
	}

	relDir, _ := filepath.Rel(mpDir, filepath.Dir(path))
	relPath := filepath.ToSlash(relDir)

	name := p.Project.Name
	if name == "" {
		name = filepath.Base(relDir)
		name = formatName(name)
	}

	url := mp.RepoURL
	if strings.Contains(url, "github.com") {
		url = strings.TrimSuffix(url, "/") + "/tree/main/" + relPath
	}

	cmd := "uvx"
	args := []string{p.Project.Name}

	return &protocol.RegistryEntry{
		ID:          mp.ID + "/" + relPath,
		Publisher:   mp.Publisher,
		Type:        "mcp",
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: p.Project.Description,
		URL:         url,
		Command:     cmd,
		Args:        args,
		Timeout:     60,
	}, nil
}
