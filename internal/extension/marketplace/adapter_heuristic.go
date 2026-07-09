package marketplace

// adapter_heuristic.go — Google Agent Skills YAML（显式声明格式）+
// package.json/pyproject.toml 依赖启发式推导（M13-bis §2.1，R7 拆分自
// adapter.go；OpenAI/Anthropic/Claude Bundle/Codex .app.json 显式清单解析
// 见 adapter.go）。

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
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Homepage     string            `json:"homepage"`
	Dependencies map[string]string `json:"dependencies"`
}

func parsePackageJSON(path, baseID string, mp protocol.Marketplace) (protocol.RegistryEntry, bool) {
	// 已有配置的 Polaris/Claude/Codex 插件已有正确的 MCP 配置，
	// 跳过自动推导 npx 命令，避免生成与现有配置冲突的错误条目。
	dir := filepath.Dir(path)
	if _, err := protocol.FindPluginManifest(dir); err == nil {
		return protocol.RegistryEntry{}, false
	}
	if _, err := protocol.FindMCPConfig(dir); err == nil {
		return protocol.RegistryEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p PackageJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}

	isMCP := strings.Contains(p.Name, "mcp")

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
	// 已有配置的原生插件已有正确的 MCP 配置（通常是
	// uv run src/main.py），跳过自动推导 uvx 命令，避免生成不存在的 PyPI 包条目（uvx <name>
	// 只适用于真正发布到 PyPI 的独立 MCP 包，不适用于本地插件工程）。
	dir := filepath.Dir(path)
	if _, err := protocol.FindPluginManifest(dir); err == nil {
		return protocol.RegistryEntry{}, false
	}
	if _, err := protocol.FindMCPConfig(dir); err == nil {
		return protocol.RegistryEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.RegistryEntry{}, false
	}
	var p PyProjectTOML
	if err := toml.Unmarshal(data, &p); err != nil {
		return protocol.RegistryEntry{}, false
	}

	isMCP := strings.Contains(p.Project.Name, "mcp")

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
