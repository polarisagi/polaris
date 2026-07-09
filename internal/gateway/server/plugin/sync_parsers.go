package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

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
