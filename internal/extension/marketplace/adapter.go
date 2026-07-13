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
// mcp.json 解析（loadMCPConfig/parseFlatMCPConfig）见本文件末尾——原属 loader.go，
// 该文件其余部分（SKILL.md 解析 / Registry 内存注册表 / Codex 插件树 GetPlugin）
// 2026-07-13 deadcode 复核确认零生产调用点已删除，唯独 loadMCPConfig 被本文件
// GetMCPConfig 使用（生产调用方：gateway/server/plugin/catalog_download.go），
// 故随 GetMCPConfig 一并保留、迁移至此。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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

// GetMCPConfig 加载并解析 .mcp.json 文件，供 server 包调用。
func GetMCPConfig(path string) (*protocol.MCPConfig, error) {
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

// parseGoogleYAML（Google Agent Skills）、PackageJSON/parsePackageJSON、
// PyProjectTOML/parsePyProjectTOML（npm/PyPI 依赖启发式推导）见
// adapter_heuristic.go（R7 拆分）。

// ─── mcp.json 解析（原 loader.go，随 GetMCPConfig 迁移保留）───────────────────

// loadMCPConfig 读取并解析 .mcp.json：优先 mcpServers（驼峰）/mcp_servers（下划线）
// 键，两者皆空时降级尝试扁平格式（server_name 直接作为根键，见 parseFlatMCPConfig）。
func loadMCPConfig(path string) (*protocol.MCPConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "loadMCPConfig", err)
	}
	var c protocol.MCPConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "loadMCPConfig", err)
	}
	if c.MCPServers == nil {
		c.MCPServers = make(map[string]protocol.MCPServerDef)
	}
	for k, v := range c.MCPServersSnake {
		c.MCPServers[k] = v
	}

	if len(c.MCPServers) == 0 {
		if flat := parseFlatMCPConfig(data); flat != nil {
			c.MCPServers = flat
		}
	}
	return &c, nil
}

// parseFlatMCPConfig 兼容"根对象直接是 server_name → def 映射"的扁平格式
// （无 mcpServers/mcp_servers 包裹层）。
func parseFlatMCPConfig(data []byte) map[string]protocol.MCPServerDef {
	var flat map[string]protocol.MCPServerDef
	if json.Unmarshal(data, &flat) != nil {
		return nil
	}
	// 过滤掉 JSON 根对象中非 MCPServerDef 的字段（如 "mcpServers" 本身为空时）
	filtered := make(map[string]protocol.MCPServerDef, len(flat))
	for k, v := range flat {
		if k == "mcpServers" || k == "mcp_servers" {
			continue // 标准 key，不视为服务器名
		}
		// 有效的服务器定义：必须有 command（stdio）或 url（HTTP/SSE）
		if v.Command != "" || v.URL != "" {
			filtered[k] = v
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	return nil
}
