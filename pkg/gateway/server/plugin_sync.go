package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
)

// SkillFrontmatter 是 SKILL.md frontmatter 的完整解析结果（agentskills.io 开放标准字段）。
type SkillFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	Tags        []string `yaml:"tags"`
	ExecMode    string   `yaml:"exec_mode"`  // "tool"（默认）| "ambient"
	RiskLevel   string   `yaml:"risk_level"` // "low" | "medium" | "high"
	Sandbox     string   `yaml:"sandbox"`    // "L1" | "L2" | "L3"
	Capability  string   `yaml:"capability"` // e.g. "read-write"
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
		return nil, err
	}
	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	// 如果 plugin.json 在 .claude-plugin / .codex-plugin 目录下，其上级目录才是插件主目录
	if b := filepath.Base(relDir); b == ".claude-plugin" || b == ".codex-plugin" {
		relDir = filepath.Dir(relDir)
	}

	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	relPath := filepath.ToSlash(relDir)

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var mcpConfig protocol.MCPConfig
	if err := json.Unmarshal(contentBytes, &mcpConfig); err != nil {
		return nil, err
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
		{".codex-plugin/plugin.json", "plugin.json"},
		{"ai-plugin.json", "ai-plugin.json"},
		{"plugin.toml", "plugin.toml"},
		{".claude-plugin/plugin.toml", "plugin.toml"},
		{"skills.yaml", "skills.yaml"},
		{"agent-manifest.yaml", "agent-manifest.yaml"},
		{"mcp.json", "mcp.json"},
		{".mcp.json", "mcp.json"},
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
func parseBundleManifest(manifestPath, manifestType, mpDir string, mp protocol.Marketplace) []protocol.RegistryEntry {
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
	}
	return entries
}

// discoverMarketplaceEntries 递归遍历市场目录，自动发现所有的插件和技能。
// 引入 Bundle Root Detection，遇到完整插件包则不再拆解其内部的子技能。
func discoverMarketplaceEntries(mpDir string, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) { //nolint:gocyclo
	var entries []protocol.RegistryEntry

	err := filepath.Walk(mpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
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

	return entries, err
}

// getGitHash 获取指定目录仓库的当前 HEAD hash
func getGitHash(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// pullOrClone 尝试执行 git pull 或 clone。
// available=false 表示仓库不可用；available=true+updated=false 表示仓库存在但无新变化。
func pullOrClone(repoURL, mpDir, gitDir string) (available bool, updated bool) {
	if _, err := os.Stat(gitDir); err == nil {
		hashBefore := getGitHash(mpDir)
		errPull := exec.Command("git", "-C", mpDir, "pull").Run()
		if errPull == nil {
			return true, hashBefore != getGitHash(mpDir)
		}
		os.RemoveAll(mpDir)
	} else {
		os.RemoveAll(mpDir)
	}

	if _, err := os.Stat(mpDir); os.IsNotExist(err) {
		errClone := exec.Command("git", "clone", "--depth", "1", repoURL, mpDir).Run()
		if errClone != nil {
			return false, false
		}
		return true, true
	}
	return false, false
}

// syncMarketplace 同步单个市场
func (s *Server) syncMarketplace(ctx context.Context, mp protocol.Marketplace, tmpDir string, localOnly bool) int {
	if mp.RepoURL == "" {
		return 0
	}

	safeID := strings.ReplaceAll(mp.ID, "/", "_")
	mpDir := filepath.Join(tmpDir, safeID)
	gitDir := filepath.Join(mpDir, ".git")

	var available, updated bool
	if localOnly {
		if _, err := os.Stat(mpDir); err == nil {
			available = true
			updated = true
		}
	} else {
		available, updated = pullOrClone(mp.RepoURL, mpDir, gitDir)
	}

	if !available {
		return 0
	}
	if !updated {
		// 仓库无新变化；若 catalog 已有条目（正常情况）则跳过，节省解析开销。
		// 若 catalog 为空（如 DB 重建），仍需重新写库，否则插件列表永久为空。
		var count int
		_ = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM extension_catalog WHERE marketplace_id=?", mp.ID).Scan(&count)
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

	return s.insertMarketplaceEntries(ctx, mp, mpDir, entries)
}

// insertMarketplaceEntries 将 entries 插入数据库，减少外层函数的圈复杂度。
func (s *Server) insertMarketplaceEntries(ctx context.Context, mp protocol.Marketplace, mpDir string, entries []protocol.RegistryEntry) int {
	syncedCount := 0
	// 对当前有更新的市场单独开启事务
	tx, err := s.db.BeginTx(ctx, nil)
	if err == nil {
		_, _ = tx.ExecContext(ctx, "DELETE FROM extension_catalog WHERE marketplace_id = ?", mp.ID)

		// 获取最新 commit hash 作为默认版本号
		cmd := exec.Command("git", "-C", mpDir, "rev-parse", "--short", "HEAD")
		out, errCmd := cmd.Output()
		var defaultVersion string
		if errCmd == nil {
			defaultVersion = strings.TrimSpace(string(out))
		}

		for i := range entries {
			e := &entries[i]
			e.Publisher = mp.Publisher
			e.TrustTier = mp.TrustTier
			if e.Version == "" && defaultVersion != "" {
				e.Version = defaultVersion
			}
			payload, _ := json.Marshal(e)

			_, _ = tx.ExecContext(ctx,
				`INSERT INTO extension_catalog(id, marketplace_id, type, name, description, publisher, trust_tier, url, payload) 
				VALUES(?,?,?,?,?,?,?,?,?)`,
				e.ID, mp.ID, e.Type, e.Name, e.Description, mp.Publisher, mp.TrustTier, e.URL, string(payload))
			syncedCount++
		}
		_ = tx.Commit()
	}

	return syncedCount
}

// SyncAllMarketplaces 后台静默同步所有可用市场并更新缓存
func (s *Server) SyncAllMarketplaces(ctx context.Context, localOnly bool) (int, error) {
	var mps []protocol.Marketplace
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, created_at FROM plugin_marketplaces WHERE enabled=1")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var m protocol.Marketplace
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.Publisher, &m.RepoURL, &m.Description, &m.IsBuiltin, &m.TrustTier, &m.Enabled, &m.CreatedAt); err == nil {
			mps = append(mps, m)
		}
	}
	rows.Close()

	tmpDir := filepath.Join(s.dataDir, "tmp", "marketplaces")
	_ = os.MkdirAll(tmpDir, 0755)

	// 首先清理已经从活跃列表中移除的孤儿市场缓存
	activeIDs := make([]any, 0, len(mps))
	queryMarks := ""
	for i, mp := range mps {
		activeIDs = append(activeIDs, mp.ID)
		if i > 0 {
			queryMarks += ","
		}
		queryMarks += "?"
	}
	if len(activeIDs) > 0 {
		delOrphanQuery := "DELETE FROM extension_catalog WHERE marketplace_id != 'builtin' AND marketplace_id NOT IN (" + queryMarks + ")"
		_, _ = s.db.ExecContext(ctx, delOrphanQuery, activeIDs...)
	} else {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM extension_catalog WHERE marketplace_id != 'builtin'")
	}

	syncedCount := 0
	for _, mp := range mps {
		syncedCount += s.syncMarketplace(ctx, mp, tmpDir, localOnly)
	}

	return syncedCount, nil
}

// parseAIPluginEntry 解析 OpenAI ai-plugin.json 格式。
// api.type=="mcp" 时映射为 mcp 条目；其余映射为 app（URL 直连）。
func parseAIPluginEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p protocol.AIPluginJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
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

// parsePluginTOMLEntry 解析 Anthropic plugin.toml（根目录或 .claude-plugin/ 下）。
func parsePluginTOMLEntry(path, mpDir string, mp protocol.Marketplace) (*protocol.RegistryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p protocol.AnthropicPluginTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Plugin.Name == "" && p.MCP.Command == "" {
		return nil, nil
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
		return nil, err
	}
	var g protocol.GoogleSkillsYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, err
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
	for i, s := range g.Skills {
		if s.Name == "" {
			continue
		}
		extType := "skill"
		if s.Command != "" {
			extType = "mcp"
		}
		entries = append(entries, protocol.RegistryEntry{
			ID:          fmt.Sprintf("%s/skill_%d", baseID, i),
			Publisher:   mp.Publisher,
			Type:        extType,
			TrustTier:   mp.TrustTier,
			Name:        s.Name,
			Description: s.Description,
			URL:         baseURL,
			Command:     s.Command,
			Args:        s.Args,
			Timeout:     60,
		})
	}
	return entries, nil
}

// handleSyncMarketplaces 刷新/同步市场
func (s *Server) handleSyncMarketplaces(w http.ResponseWriter, r *http.Request) {
	localOnly := r.URL.Query().Get("local_only") == "true"
	slog.Info("polaris-server: manual sync marketplaces triggered", "local_only", localOnly)
	syncedCount, err := s.SyncAllMarketplaces(r.Context(), localOnly)
	if err != nil {
		slog.Error("polaris-server: manual sync marketplaces failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("polaris-server: manual sync marketplaces finished", "synced_count", syncedCount)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "synced", "synced_count": syncedCount})
}
