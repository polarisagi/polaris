// Package plugin 的本文件收录第三方生态清单格式适配器（R7 文件行数治理，
// 从 sync_parsers.go 拆出，2026-07-07）：OpenAI ai-plugin.json、Anthropic
// plugin.toml、Google skills.yaml/agent-manifest.yaml、npm package.json、
// Python pyproject.toml。与 sync_parsers.go 保留的原生格式（SKILL.md/
// plugin.json/mcp.json）解析器 + discoverMarketplaceEntries 发现编排逻辑
// 职责边界一致：本文件只做"格式适配"，不参与目录遍历/Bundle Root 探测。
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

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

// parsePackageJSONEntry 解析 npm package.json，仅当依赖或包名暗示 MCP Server 时才生成条目。
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

// parsePyProjectTOMLEntry 解析 Python pyproject.toml，仅当依赖或包名暗示 MCP Server 时才生成条目。
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
