package pluginspec

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	RuleAppShape        = "codex.apps.shape"
	RuleUserConfigShape = "claude.userConfig.strict"
	RuleUserConfigKey   = "claude.userConfig.key"
	RuleChannelShape    = "claude.channels.strict"
	RuleChannelServer   = "claude.channels.server"
	RuleDependencyShape = "claude.dependencies.shape"
)

var userConfigKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AppBinding Codex .app.json 的一条应用绑定：插件对平台上已注册连接器的引用，
// 不是连接定义（ADR-0103 决策四）。解析与绑定发生在安装层。
type AppBinding struct {
	Alias       string `json:"alias"`
	ConnectorID string `json:"connector_id"`
	Source      string `json:"source"`
}

// loadApps Codex apps：清单 apps 指向 .app.json，缺省 ./.app.json。
func loadApps(root string, docs []manifestDoc, ds *diagnostics) []AppBinding {
	var files []string
	for _, doc := range docsWithField(docs, func(w manifestWire) json.RawMessage { return w.Apps }) {
		var rel string
		if json.Unmarshal(doc.wire.Apps, &rel) != nil {
			ds.errorf("app", doc.path, RuleAppShape, "apps must be a path to .app.json")
			continue
		}
		abs, rule, err := resolveComponentPath(root, rel, false)
		if err != nil {
			ds.errorf("app", doc.path, rule, "apps path %q: %v", rel, err)
			continue
		}
		files = append(files, abs)
	}
	if len(files) == 0 {
		if f, ok := existingFile(root, ".app.json"); ok {
			files = append(files, f)
		}
	}
	out := make([]AppBinding, 0, len(files))
	for _, f := range files {
		out = append(out, parseAppFile(f, ds)...)
	}
	return out
}

func parseAppFile(path string, ds *diagnostics) []AppBinding {
	raw, err := os.ReadFile(path)
	if err != nil {
		ds.errorf("app", path, RuleAppShape, "read failed: %v", err)
		return nil
	}
	var doc struct {
		Apps map[string]struct {
			ID string `json:"id"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		ds.errorf("app", path, RuleAppShape, "expected {\"apps\":{\"<alias>\":{\"id\":\"...\"}}}: %v", err)
		return nil
	}
	aliases := make([]string, 0, len(doc.Apps))
	for a := range doc.Apps {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	out := make([]AppBinding, 0, len(aliases))
	for _, alias := range aliases {
		id := strings.TrimSpace(doc.Apps[alias].ID)
		if alias == "" || id == "" {
			ds.errorf("app", path, RuleAppShape, "app %q must have a non-empty id", alias)
			continue
		}
		out = append(out, AppBinding{Alias: alias, ConnectorID: id, Source: path})
	}
	return out
}

// UserConfigOption Claude userConfig 单项（严格对象：未知键使该项失败）。
type UserConfigOption struct {
	Key         string          `json:"key"`
	Type        string          `json:"type"` // string / number / boolean / directory / file
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Options     []string        `json:"options,omitempty"`
	Multiple    bool            `json:"multiple,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	Min         *float64        `json:"min,omitempty"`
	Max         *float64        `json:"max,omitempty"`
}

// Channel Claude 插件消息通道：绑定插件内一个 MCP 服务器作为会话消息来源。
type Channel struct {
	Server      string             `json:"server"`
	DisplayName string             `json:"display_name,omitempty"`
	UserConfig  []UserConfigOption `json:"user_config,omitempty"`
}

// Dependency Claude 插件依赖："name"、"name@marketplace" 或对象。
type Dependency struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace,omitempty"`
	Version     string `json:"version,omitempty"`
}

func loadUserConfigAndChannels(docs []manifestDoc, ds *diagnostics) ([]UserConfigOption, []Channel) {
	var opts []UserConfigOption
	var chans []Channel
	for _, doc := range docs {
		if doc.format != FormatClaude {
			continue
		}
		if len(doc.wire.UserConfig) > 0 {
			opts = append(opts, parseUserConfigOptions(doc.wire.UserConfig, doc.path, ds)...)
		}
		if len(doc.wire.Channels) > 0 {
			chans = append(chans, parseChannels(doc.wire.Channels, doc.path, ds)...)
		}
	}
	return opts, chans
}

// parseUserConfigOptions 保留 JSON 声明顺序（配置对话框按作者顺序展示）。
func parseUserConfigOptions(raw json.RawMessage, path string, ds *diagnostics) []UserConfigOption {
	keys, values, ok := orderedObject(raw)
	if !ok {
		ds.errorf("user_config", path, RuleUserConfigShape, "userConfig must be an object")
		return nil
	}
	var out []UserConfigOption
	for i, key := range keys {
		if !userConfigKeyPattern.MatchString(key) {
			ds.errorf("user_config", path, RuleUserConfigKey, "key %q must be letters, digits and underscores, not starting with a digit", key)
			continue
		}
		if opt, ok := parseUserConfigOption(key, values[i], path, ds); ok {
			out = append(out, opt)
		}
	}
	return out
}

func parseUserConfigOption(key string, raw json.RawMessage, path string, ds *diagnostics) (UserConfigOption, bool) {
	opt := UserConfigOption{Key: key}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct {
		Type        *string          `json:"type"`
		Title       *string          `json:"title"`
		Description *string          `json:"description"`
		Required    *bool            `json:"required"`
		Default     *json.RawMessage `json:"default"`
		Options     *[]string        `json:"options"`
		Multiple    *bool            `json:"multiple"`
		Sensitive   *bool            `json:"sensitive"`
		Min         **float64        `json:"min"`
		Max         **float64        `json:"max"`
	}{&opt.Type, &opt.Title, &opt.Description, &opt.Required, &opt.Default, &opt.Options,
		&opt.Multiple, &opt.Sensitive, &opt.Min, &opt.Max}); err != nil {
		ds.errorf("user_config", path, RuleUserConfigShape, "option %q: %v", key, err)
		return opt, false
	}
	return opt, validateUserConfigOption(opt, path, ds)
}

func validateUserConfigOption(opt UserConfigOption, path string, ds *diagnostics) bool {
	switch opt.Type {
	case "string", "number", "boolean", "directory", "file":
	default:
		ds.errorf("user_config", path, RuleUserConfigShape, "option %q: type must be string/number/boolean/directory/file", opt.Key)
		return false
	}
	if opt.Title == "" || opt.Description == "" {
		ds.errorf("user_config", path, RuleUserConfigShape, "option %q: title and description are required", opt.Key)
		return false
	}
	if len(opt.Options) == 0 {
		return true
	}
	if opt.Type != "string" || opt.Multiple || opt.Sensitive {
		ds.errorf("user_config", path, RuleUserConfigShape, "option %q: options only apply to a single non-sensitive string", opt.Key)
		return false
	}
	for _, o := range opt.Options {
		if n := utf8.RuneCountInString(o); n < 1 || n > 64 {
			ds.errorf("user_config", path, RuleUserConfigShape, "option %q: each choice must be 1-64 characters", opt.Key)
			return false
		}
	}
	return true
}

func parseChannels(raw json.RawMessage, path string, ds *diagnostics) []Channel {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		ds.errorf("channel", path, RuleChannelShape, "channels must be an array")
		return nil
	}
	var out []Channel
	for _, item := range items {
		var w struct {
			Server      string          `json:"server"`
			DisplayName string          `json:"displayName"`
			UserConfig  json.RawMessage `json:"userConfig"`
		}
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&w); err != nil || w.Server == "" {
			ds.errorf("channel", path, RuleChannelShape, "channel entry invalid (server required, no unknown keys): %v", err)
			continue
		}
		ch := Channel{Server: w.Server, DisplayName: w.DisplayName}
		if len(w.UserConfig) > 0 {
			ch.UserConfig = parseUserConfigOptions(w.UserConfig, path, ds)
		}
		out = append(out, ch)
	}
	return out
}

// validateChannels channel.server 必须是本插件声明的 MCP 服务器。
func validateChannels(p *Plugin, ds *diagnostics) {
	servers := map[string]bool{}
	for _, s := range p.MCPServers {
		servers[s.Name] = true
	}
	kept := p.Channels[:0]
	for _, ch := range p.Channels {
		if !servers[ch.Server] {
			ds.errorf("channel", p.Root, RuleChannelServer, "channel server %q is not one of this plugin's MCP servers", ch.Server)
			continue
		}
		kept = append(kept, ch)
	}
	p.Channels = kept
}

// mergeUserConfig .mcpb 包内选项并入插件 userConfig；同名以插件清单为准。
func mergeUserConfig(base, extra []UserConfigOption) []UserConfigOption {
	seen := map[string]bool{}
	for _, o := range base {
		seen[o.Key] = true
	}
	for _, o := range extra {
		if !seen[o.Key] {
			base = append(base, o)
			seen[o.Key] = true
		}
	}
	return base
}

func loadDependencies(docs []manifestDoc, ds *diagnostics) []Dependency {
	var out []Dependency
	for _, doc := range docs {
		if doc.format != FormatClaude || len(doc.wire.Dependencies) == 0 {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(doc.wire.Dependencies, &items); err != nil {
			ds.errorf("dependency", doc.path, RuleDependencyShape, "dependencies must be an array")
			continue
		}
		for _, item := range items {
			if d, ok := parseDependency(item); ok {
				out = append(out, d)
			} else {
				ds.errorf("dependency", doc.path, RuleDependencyShape, "dependency %s is invalid", string(item))
			}
		}
	}
	return out
}

func parseDependency(raw json.RawMessage) (Dependency, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		name, mp, _ := strings.Cut(s, "@")
		return Dependency{Name: name, Marketplace: mp}, name != ""
	}
	var d Dependency
	if json.Unmarshal(raw, &d) == nil && d.Name != "" {
		return d, true
	}
	return Dependency{}, false
}

// orderedObject 按源码顺序返回 JSON 对象的键与值（encoding/json 解码到 map 会丢失顺序）。
func orderedObject(raw json.RawMessage) ([]string, []json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	var vals []json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		keys = append(keys, key)
		vals = append(vals, v)
	}
	return keys, vals, true
}
