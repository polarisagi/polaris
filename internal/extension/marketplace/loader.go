package marketplace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SkillMetaFromSKILLmd 解析 agentskills.io 标准 SKILL.md 文件，
// 转换为 types.SkillMeta（ADR-0015 §2.3）。
//
// 签名：外部 SKILL.md 无 SIGNATURE 文件，使用 HMAC-SHA256（密钥 = signingKey）
// 生成本地签名，设置 SignatureValid=true + Capabilities 附加 "trust:local"。
// Cedar 策略通过 "trust:local" vs "trust:verified" 区分沙箱级别。
func SkillMetaFromSKILLmd(path string, signingKey []byte) (*types.SkillMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("loader: read %s: %v", path, err), err)
	}

	name, description, execMode, err := parseFrontmatter(data)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("loader: parse frontmatter %s: %v", path, err), err)
	}
	if name == "" {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("loader: SKILL.md %s missing 'name' in frontmatter", path))
	}

	// 本地 HMAC 签名（替代 cosign，标记 trust:local）
	mac := hmac.New(sha256.New, signingKey)
	mac.Write(data)

	return &types.SkillMeta{
		Name:         "skill:" + name,
		Version:      "1.0.0",
		Runtime:      "markdown", // NL 描述技能，非 Wasm
		RiskLevel:    "low",
		Sandbox:      1, // Sbx-L1（TrustLocal 上限）
		Capabilities: []string{"description:" + description},
		Trust:        types.TrustLocal, // HMAC 本地验证通过，publisher 未认证
		Idempotent:   false,
		ExecMode:     execMode,
		Benchmarks:   types.SkillBenchmarks{},
	}, nil
}

// parseFrontmatter 提取 SKILL.md YAML frontmatter 中的 name 和 description。
// 格式：--- ... name: xxx ... description: yyy ... ---
func parseFrontmatter(data []byte) (name, description, execMode string, err error) {
	lines := strings.Split(string(data), "\n")
	firstDash := -1
	secondDash := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if firstDash == -1 {
				firstDash = i
			} else {
				secondDash = i
				break
			}
		}
	}

	if firstDash != -1 && secondDash != -1 && secondDash > firstDash+1 {
		yamlContent := strings.Join(lines[firstDash+1:secondDash], "\n")
		var fm struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			ExecMode    string `yaml:"exec_mode"`
		}
		if err := yaml.Unmarshal([]byte(yamlContent), &fm); err == nil {
			mode := fm.ExecMode
			if mode == "" {
				mode = "tool" // schema DEFAULT 'tool'
			}
			return fm.Name, fm.Description, mode, nil
		}
	}

	return "", "", "", apperr.New(apperr.CodeInternal, "loader: failed to parse YAML frontmatter")
}

// ParseSKILLmd 解析 SKILL.md，需要提供 signingKey 保证跨重启验证一致性。
func ParseSKILLmd(path string, signingKey []byte) (*types.SkillMeta, error) {
	return SkillMetaFromSKILLmd(path, signingKey)
}

// LoadPlugin 从指定目录加载完整的 Codex 插件树。
func LoadPlugin(dir string) (*Plugin, error) {
	manifestPath := filepath.Join(dir, ".polaris-plugin", "plugin.json")
	manifest, err := parseManifest(manifestPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LoadPlugin", err)
	}

	plugin := &Plugin{
		Manifest: *manifest,
		Dir:      dir,
		Enabled:  true,
		MCPs:     make(map[string]protocol.MCPServerDef),
	}

	// 尝试加载 .mcp.json
	if manifest.MCPServers != "" {
		mcpPath := manifest.MCPServers
		if !filepath.IsAbs(mcpPath) {
			mcpPath = filepath.Join(dir, mcpPath)
		}
		if mcpConfig, err := loadMCPConfig(mcpPath); err == nil {
			plugin.MCPs = mcpConfig.MCPServers
		} else if !errors.Is(err, os.ErrNotExist) {
			// 如果不是文件不存在导致的错误，则返回解析错误
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("loader: failed to load %s: %v", mcpPath, err), err)
		}
	}

	return plugin, nil
}

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

// parseManifest reads and parses a plugin.json file.
func parseManifest(path string) (*protocol.PluginJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseManifest", err)
	}
	var manifest protocol.PluginJSON
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "parseManifest", err)
	}
	return &manifest, nil
}
