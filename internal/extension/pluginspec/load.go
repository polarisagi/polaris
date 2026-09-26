package pluginspec

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// agentPluginsNamePattern agent-plugins 1.0：小写字母数字，连字符/点分隔，不得连续。
var agentPluginsNamePattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)

// Plugin 归一化插件：三种清单 + 默认布局合并后的唯一内部模型（ADR-0103 决策二）。
type Plugin struct {
	Root    string           `json:"root"`
	Formats []ManifestFormat `json:"formats"`

	Name           string          `json:"name"`
	DisplayName    string          `json:"display_name,omitempty"`
	Version        string          `json:"version,omitempty"`
	Description    string          `json:"description,omitempty"`
	Author         *Author         `json:"author,omitempty"`
	Homepage       string          `json:"homepage,omitempty"`
	Repository     string          `json:"repository,omitempty"`
	License        string          `json:"license,omitempty"`
	Keywords       []string        `json:"keywords,omitempty"`
	DefaultEnabled bool            `json:"default_enabled"`
	Interface      *CodexInterface `json:"interface,omitempty"`

	Skills       []*Skill               `json:"skills,omitempty"` // 含由 commands 转换的技能
	Agents       []AgentFile            `json:"agents,omitempty"`
	Hooks        []HookSource           `json:"hooks,omitempty"`
	MCPServers   []MCPServer            `json:"mcp_servers,omitempty"`
	Apps         []AppBinding           `json:"apps,omitempty"`
	UserConfig   []UserConfigOption     `json:"user_config,omitempty"`
	Channels     []Channel              `json:"channels,omitempty"`
	Dependencies []Dependency           `json:"dependencies,omitempty"`
	Unsupported  []UnsupportedComponent `json:"unsupported,omitempty"`

	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// LoadOptions 调用方提供的外部上下文。
type LoadOptions struct {
	// FallbackName 无清单或清单无 name 时的插件名（Claude：取市场条目名，否则目录名）。
	FallbackName string
	// RemoteBundles 安装层已下载的远程 MCP 包（https URL → 本地 .mcpb 路径），见 ListRemoteBundles。
	RemoteBundles map[string]string
}

// ErrNotAPlugin 目录既无任何清单也无任何默认布局组件。
var ErrNotAPlugin = apperr.New(apperr.CodeInvalidInput, "pluginspec: directory is not a plugin")

// ErrInvalidPlugin 插件级致命错误（名称非法等），组件级错误不走此错误而记入 Diagnostics。
var ErrInvalidPlugin = apperr.New(apperr.CodeInvalidInput, "pluginspec: invalid plugin")

// Load 解析插件目录。返回错误仅限插件级致命问题；组件级问题记入 Plugin.Diagnostics。
func Load(root string, opts LoadOptions) (*Plugin, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec.Load", err)
	}
	var ds diagnostics
	docs := readManifests(abs, &ds)
	p := &Plugin{Root: abs, DefaultEnabled: true}
	for _, d := range docs {
		p.Formats = append(p.Formats, d.format)
	}
	applyIdentity(p, docs, opts, &ds)
	if !validPluginName(p.Name) {
		ds.errorf("manifest", abs, RuleManifestName, "plugin name %q is invalid", p.Name)
		p.Diagnostics = ds
		return p, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec.Load: "+p.Name, ErrInvalidPlugin)
	}
	if !agentPluginsNamePattern.MatchString(p.Name) || len(p.Name) > 64 {
		ds.warnf("manifest", abs, RuleManifestName, "name %q should be 1-64 lowercase letters/digits separated by '-' or '.'", p.Name)
	}
	loadComponents(p, docs, opts, &ds)
	p.Diagnostics = ds
	if len(docs) == 0 && p.isEmpty() {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec.Load: "+abs, ErrNotAPlugin)
	}
	return p, nil
}

func loadComponents(p *Plugin, docs []manifestDoc, opts LoadOptions, ds *diagnostics) {
	p.Skills = loadSkills(p.Root, p.Name, docs, ds)
	p.Skills = append(p.Skills, loadCommands(p.Root, docs, ds)...)
	p.Agents = loadAgents(p.Root, docs, ds)
	p.Hooks = loadHooks(p.Root, docs, ds)
	var bundleOptions []UserConfigOption
	p.MCPServers, bundleOptions = loadMCPServers(p.Root, docs, opts, ds)
	p.Apps = loadApps(p.Root, docs, ds)
	p.UserConfig, p.Channels = loadUserConfigAndChannels(docs, ds)
	p.UserConfig = mergeUserConfig(p.UserConfig, bundleOptions)
	validateChannels(p, ds)
	p.Dependencies = loadDependencies(docs, ds)
	p.Unsupported = detectUnsupported(p.Root, docs)
}

func (p *Plugin) isEmpty() bool {
	return len(p.Skills) == 0 && len(p.Agents) == 0 && len(p.Hooks) == 0 && len(p.MCPServers) == 0 && len(p.Apps) == 0
}

// HasErrors 是否存在组件级错误（UI 展示用；不影响其余组件的加载）。
func (p *Plugin) HasErrors() bool {
	for _, d := range p.Diagnostics {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// applyIdentity 身份字段按清单优先级取第一个非空值（agent-plugins > claude > codex）。
func applyIdentity(p *Plugin, docs []manifestDoc, opts LoadOptions, ds *diagnostics) {
	for _, d := range docs {
		w := d.wire
		p.Name = firstNonEmpty(p.Name, w.Name)
		p.DisplayName = firstNonEmpty(p.DisplayName, w.DisplayName)
		p.Version = firstNonEmpty(p.Version, w.Version)
		p.Description = firstNonEmpty(p.Description, w.Description)
		p.Homepage = firstNonEmpty(p.Homepage, w.Homepage)
		p.Repository = firstNonEmpty(p.Repository, parseRepository(w.Repository))
		p.License = firstNonEmpty(p.License, w.License)
		if p.Author == nil {
			p.Author = parseAuthor(w.Author, d.path, ds)
		}
		if len(p.Keywords) == 0 {
			p.Keywords = w.Keywords
		}
		if p.Interface == nil && w.Interface != nil {
			p.Interface = w.Interface
		}
		if w.DefaultEnabled != nil && d.format == FormatClaude {
			p.DefaultEnabled = *w.DefaultEnabled
		}
	}
	p.Name = strings.TrimSpace(firstNonEmpty(p.Name, opts.FallbackName, filepath.Base(p.Root)))
	if p.DisplayName == "" && p.Interface != nil {
		p.DisplayName = p.Interface.DisplayName
	}
}

// validPluginName Claude 规则：非空，不含空白、@、:、路径分隔符、控制与双向格式字符。
func validPluginName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || isBidiControl(r) || strings.ContainsRune(`@:/\`, r) {
			return false
		}
	}
	return true
}

// docsWithField 返回声明了某组件字段的清单（按优先级顺序）。
func docsWithField(docs []manifestDoc, pick func(manifestWire) json.RawMessage) []manifestDoc {
	var out []manifestDoc
	for _, d := range docs {
		if raw := pick(d.wire); len(raw) > 0 && string(raw) != "null" {
			out = append(out, d)
		}
	}
	return out
}

// rawPaths 解码 "路径或路径数组" 字段。
func rawPaths(raw json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many, true
	}
	return nil, false
}
