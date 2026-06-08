package marketplace

// adapter.go — 多厂商插件清单解析适配器（M13-bis §2.1）
//
// 支持格式：
//   - OpenAI   ai-plugin.json（ChatGPT Plugins 旧格式，兼容保留）
//   - OpenAI   .app.json（Codex connector/app 格式）
//   - Anthropic .claude-plugin/plugin.toml 或 plugin.toml
//   - Anthropic .claude-plugin/plugin.json（Claude 原生 Bundle）
//   - Google    skills.yaml / agent-manifest.yaml
//
// Polaris 原生格式（SKILL.md / plugin.json / mcp.json）由 loader.go 负责，
// 此文件不重复处理，避免 discoverMarketplaceEntries Walk 产生重复条目。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ParseManifestDir 探测 dir 中所有已知的外部厂商清单格式并返回 RegistryEntry 列表。
// mpRoot 为市场克隆根目录（用于计算相对路径 ID）；Bundle 安装时传空字符串。
// 一个目录可能返回多个条目（如同时含 ai-plugin.json 和 SKILL.md）。
func ParseManifestDir(dir, mpRoot string, mp protocol.Marketplace) ([]protocol.RegistryEntry, error) {
	relPath := "."
	if mpRoot != "" {
		if r, err := filepath.Rel(mpRoot, dir); err == nil {
			relPath = filepath.ToSlash(r)
		}
	}
	baseID := mp.ID + "/" + relPath

	var entries []protocol.RegistryEntry

	if e, ok := parseAIPlugin(dir, baseID, mp); ok {
		entries = append(entries, e)
	}
	if e, ok := parseAnthropicTOML(filepath.Join(dir, ".claude-plugin", "plugin.toml"), baseID, mp); ok {
		entries = append(entries, e)
	}
	if e, ok := parseAnthropicTOML(filepath.Join(dir, "plugin.toml"), baseID, mp); ok {
		entries = append(entries, e)
	}
	if e, ok := parseClaudePluginJSON(dir, baseID, mp); ok {
		entries = append(entries, e)
	}
	if es := parseGoogleYAML(dir, baseID, mp, "skills.yaml"); len(es) > 0 {
		entries = append(entries, es...)
	}
	if es := parseGoogleYAML(dir, baseID, mp, "agent-manifest.yaml"); len(es) > 0 {
		entries = append(entries, es...)
	}
	if es := parseAppJSON(dir, baseID, mp); len(es) > 0 {
		entries = append(entries, es...)
	}
	if e, ok := parsePackageJSON(filepath.Join(dir, "package.json"), baseID, mp); ok {
		entries = append(entries, e)
	}
	if e, ok := parsePyProjectTOML(filepath.Join(dir, "pyproject.toml"), baseID, mp); ok {
		entries = append(entries, e)
	}

	return entries, nil
}

// LoadMCPConfig 加载并解析 .mcp.json 文件，供 server 包调用。
func LoadMCPConfig(path string) (*protocol.MCPConfig, error) {
	return loadMCPConfig(path)
}

// ─── OpenAI ──────────────────────────────────────────────────────────────────

func parseAIPlugin(dir, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "ai-plugin.json"))
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p protocol.AIPluginJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}

	name := p.NameForHuman
	if name == "" {
		name = p.NameForModel
	}
	desc := p.DescriptionForHuman
	if desc == "" {
		desc = p.DescriptionForModel
	}
	if name == "" {
		return protocol.RegistryEntry{}, false
	}

	// OpenAI ai-plugin.json 的 api.type 一般是 "openapi"（REST API），极少数声明 "mcp"。
	// - openapi: 注册为 "app" 类型，URL 指向 OpenAPI spec；不生成 command（非 stdio 进程）
	// - mcp: 服务器是 MCP HTTP 端点，URL 作为 HTTP transport 的 endpoint
	extType := "app"
	transport := ""
	entryURL := p.API.URL
	if strings.EqualFold(p.API.Type, "mcp") {
		extType = "mcp"
		transport = "http" // MCP HTTP transport，非 stdio
	}

	return protocol.RegistryEntry{
		ID:          baseID,
		Publisher:   mp.Publisher,
		Type:        extType,
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: desc,
		Transport:   transport,
		URL:         entryURL,
		Homepage:    p.LegalInfoURL,
		// Command 留空：OpenAI 插件是 HTTP 服务，不是本地 stdio 进程
		Timeout: 60,
	}, true
}

// ─── Anthropic TOML ──────────────────────────────────────────────────────────

func parseAnthropicTOML(tomlPath, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p protocol.AnthropicPluginTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}
	if p.Plugin.Name == "" && p.MCP.Command == "" {
		return protocol.RegistryEntry{}, false
	}

	extType := "mcp"
	if p.MCP.Command == "" {
		extType = "plugin"
	}

	return protocol.RegistryEntry{
		ID:          baseID,
		Publisher:   mp.Publisher,
		Type:        extType,
		TrustTier:   mp.TrustTier,
		Name:        p.Plugin.Name,
		Description: p.Plugin.Description,
		Command:     p.MCP.Command,
		Args:        p.MCP.Args,
		Env:         p.MCP.Env,
		Timeout:     60,
	}, true
}

// ─── Claude Plugin JSON（Anthropic 原生 Bundle）──────────────────────────────

func parseClaudePluginJSON(dir, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	// 仅处理 .claude-plugin/plugin.json 或 .codex-plugin/plugin.json；跳过已有 .polaris-plugin 的目录（原生格式优先）
	if _, err := os.Stat(filepath.Join(dir, ".polaris-plugin")); err == nil {
		return protocol.RegistryEntry{}, false
	}
	
	var pPath string
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		pPath = filepath.Join(dir, ".claude-plugin", "plugin.json")
	} else if _, err := os.Stat(filepath.Join(dir, ".codex-plugin", "plugin.json")); err == nil {
		pPath = filepath.Join(dir, ".codex-plugin", "plugin.json")
	} else {
		return protocol.RegistryEntry{}, false
	}
	
	data, err := os.ReadFile(pPath)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p protocol.PluginJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}
	if p.Name == "" {
		return protocol.RegistryEntry{}, false
	}
	e := protocol.RegistryEntry{
		ID:          baseID,
		Publisher:   mp.Publisher,
		Type:        "plugin",
		TrustTier:   mp.TrustTier,
		Name:        p.Name,
		Description: p.Description,
		Homepage:    p.Homepage,
		Timeout:     60,
	}
	if p.Interface != nil {
		e.DisplayName = p.Interface.DisplayName
		e.ShortDescription = p.Interface.ShortDescription
		e.Icon = p.Interface.IconSmall
	}
	return e, true
}

// ─── OpenAI Codex .app.json ──────────────────────────────────────────────────

// parseAppJSON 解析 Codex .app.json connector/app 映射格式。
// 每个 AppDef 生成一条 type="app" 的 RegistryEntry。
func parseAppJSON(dir, baseID string, mp protocol.Marketplace) []protocol.RegistryEntry {
	data, err := os.ReadFile(filepath.Join(dir, ".app.json"))
	if err != nil {
		return nil
	}
	var cfg protocol.AppJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	entries := make([]protocol.RegistryEntry, 0, len(cfg.Apps))
	for i, app := range cfg.Apps {
		if app.Name == "" {
			continue
		}
		entries = append(entries, protocol.RegistryEntry{
			ID:          baseID + "/app_" + strconv.Itoa(i),
			Publisher:   mp.Publisher,
			Type:        "app",
			TrustTier:   mp.TrustTier,
			Name:        app.Name,
			Description: app.Description,
			URL:         app.URL,
			Command:     app.Command,
			Timeout:     60,
		})
	}
	return entries
}

// ─── Google Agent Skills YAML ────────────────────────────────────────────────

func parseGoogleYAML(dir, baseID string, mp protocol.Marketplace, filename string) []protocol.RegistryEntry {
	data, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return nil
	}
	var g protocol.GoogleSkillsYAML
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil
	}

	// 单条目（顶层 name）
	if g.Name != "" && len(g.Skills) == 0 {
		extType := "skill"
		command := ""
		args := g.Args
		if g.Command != "" {
			extType = "mcp"
			command = g.Command
		}
		return []protocol.RegistryEntry{{
			ID:          baseID,
			Publisher:   mp.Publisher,
			Type:        extType,
			TrustTier:   mp.TrustTier,
			Name:        g.Name,
			Description: g.Description,
			Command:     command,
			Args:        args,
			Timeout:     60,
		}}
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
			ID:          baseID + "/skill_" + strconv.Itoa(i),
			Publisher:   mp.Publisher,
			Type:        extType,
			TrustTier:   mp.TrustTier,
			Name:        s.Name,
			Description: s.Description,
			Command:     s.Command,
			Args:        s.Args,
			Timeout:     60,
		})
	}
	return entries
}

type PackageJSON struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Version     string            `json:"version"`
	Homepage    string            `json:"homepage"`
	Dependencies map[string]string `json:"dependencies"`
}

func parsePackageJSON(path, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p PackageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}

	isMCP := false
	if strings.Contains(p.Name, "mcp") {
		isMCP = true
	}
	for k := range p.Dependencies {
		if strings.Contains(k, "modelcontextprotocol") || k == "mcp" {
			isMCP = true
			break
		}
	}
	if !isMCP {
		return protocol.RegistryEntry{}, false
	}

	name := p.Name
	if name == "" {
		return protocol.RegistryEntry{}, false
	}

	cmd := "npx"
	args := []string{"-y", p.Name + "@latest"}

	return protocol.RegistryEntry{
		ID:          baseID,
		Publisher:   mp.Publisher,
		Type:        "mcp",
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: p.Description,
		Homepage:    p.Homepage,
		Command:     cmd,
		Args:        args,
		Timeout:     60,
	}, true
}

type PyProjectTOML struct {
	Project struct {
		Name         string   `toml:"name"`
		Description  string   `toml:"description"`
		Version      string   `toml:"version"`
		Dependencies []string `toml:"dependencies"`
	} `toml:"project"`
}

func parsePyProjectTOML(path, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p PyProjectTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}

	isMCP := false
	if strings.Contains(p.Project.Name, "mcp") {
		isMCP = true
	}
	for _, dep := range p.Project.Dependencies {
		if strings.Contains(dep, "mcp") {
			isMCP = true
			break
		}
	}
	if !isMCP {
		return protocol.RegistryEntry{}, false
	}

	name := p.Project.Name
	if name == "" {
		return protocol.RegistryEntry{}, false
	}

	cmd := "uvx"
	args := []string{p.Project.Name}

	return protocol.RegistryEntry{
		ID:          baseID,
		Publisher:   mp.Publisher,
		Type:        "mcp",
		TrustTier:   mp.TrustTier,
		Name:        name,
		Description: p.Project.Description,
		Command:     cmd,
		Args:        args,
		Timeout:     60,
	}, true
}
