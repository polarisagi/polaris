package pluginspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 市场目录（ADR-0103 决策七）：Claude `.claude-plugin/marketplace.json` 与 Codex
// `.agents/plugins/marketplace.json`（Codex 亦读取前者作为旧位置）。二者同在时合并，同名条目以
// Claude 文件为主、补入 Codex 的 policy / category。
const (
	RuleMarketplaceShape  = "marketplace.shape"
	RuleMarketplaceName   = "marketplace.name"
	RuleMarketplaceEntry  = "marketplace.entry"
	RuleMarketplaceSource = "marketplace.source"
)

// 插件来源类型（Claude plugin sources + Codex local）。
const (
	SourceRelative  = "relative"
	SourceGitHub    = "github"
	SourceURL       = "url"
	SourceGitSubdir = "git-subdir"
	SourceNPM       = "npm"
	SourceArchive   = "archive"
)

// Marketplace 归一化市场目录。
type Marketplace struct {
	Root        string   `json:"root"`
	Files       []string `json:"files"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Owner       *Author  `json:"owner,omitempty"`
	PluginRoot  string   `json:"plugin_root,omitempty"`
	// AllowCrossMarketplaceDeps 本市场插件可从哪些其他市场安装依赖（Claude allowCrossMarketplaceDependenciesOn）。
	AllowCrossMarketplaceDeps []string           `json:"allow_cross_marketplace_deps,omitempty"`
	Renames                   map[string]*string `json:"renames,omitempty"`
	Plugins                   []MarketplaceEntry `json:"plugins"`
	Diagnostics               []Diagnostic       `json:"diagnostics,omitempty"`
}

// MarketplaceEntry 市场中的一个插件条目。Manifest 为条目原文（条目可承载全部 plugin.json 字段，
// 安装时经 LoadOptions.Entry 与插件自身清单按 strict 规则组合）。
type MarketplaceEntry struct {
	Name           string            `json:"name"`
	DisplayName    string            `json:"display_name,omitempty"`
	Description    string            `json:"description,omitempty"`
	Version        string            `json:"version,omitempty"`
	Category       string            `json:"category,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	Keywords       []string          `json:"keywords,omitempty"`
	Author         *Author           `json:"author,omitempty"`
	Homepage       string            `json:"homepage,omitempty"`
	Source         PluginSource      `json:"source"`
	Strict         bool              `json:"strict"`
	DefaultEnabled *bool             `json:"default_enabled,omitempty"`
	Dependencies   []Dependency      `json:"dependencies,omitempty"`
	Policy         *CodexPolicy      `json:"policy,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Manifest       json.RawMessage   `json:"manifest,omitempty"`
}

// CodexPolicy Codex 条目安装策略。
type CodexPolicy struct {
	Installation   string `json:"installation,omitempty"`   // AVAILABLE / INSTALLED_BY_DEFAULT / NOT_AVAILABLE
	Authentication string `json:"authentication,omitempty"` // ON_INSTALL / ON_USE
}

// PluginSource 归一化插件来源。Relative 的 Path 已解析为市场根内的绝对路径。
type PluginSource struct {
	Type     string `json:"type"`
	Path     string `json:"path,omitempty"` // relative：绝对路径；git-subdir：仓库内子目录
	Repo     string `json:"repo,omitempty"` // github owner/repo
	URL      string `json:"url,omitempty"`
	Ref      string `json:"ref,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Package  string `json:"package,omitempty"`
	Version  string `json:"version,omitempty"`
	Registry string `json:"registry,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Headers 下载 archive 时发送的请求头（来自条目 headers；路由/身份类头已剔除）。
	Headers map[string]string `json:"headers,omitempty"`
}

// ErrNotAMarketplace 目录下没有任何市场文件。
var ErrNotAMarketplace = apperr.New(apperr.CodeInvalidInput, "pluginspec: directory is not a marketplace")

// GetMarketplace 读取市场根目录（含 .claude-plugin/ 或 .agents/plugins/ 的目录）。
// 条目逐个校验，单个条目无效不影响整个市场（两家一致）。
func GetMarketplace(root string) (*Marketplace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec.GetMarketplace", err)
	}
	var ds diagnostics
	m := &Marketplace{Root: abs}
	for _, rel := range []string{filepath.Join(".claude-plugin", "marketplace.json"), filepath.Join(".agents", "plugins", "marketplace.json")} {
		path := filepath.Join(abs, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		m.Files = append(m.Files, path)
		mergeMarketplace(m, parseMarketplaceFile(abs, path, raw, &ds))
	}
	if len(m.Files) == 0 {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "pluginspec.GetMarketplace: "+abs, ErrNotAMarketplace)
	}
	if !validMarketplaceName(m.Name) {
		ds.errorf("marketplace", m.Files[0], RuleMarketplaceName, "marketplace name %q is invalid", m.Name)
	}
	m.Diagnostics = ds
	return m, nil
}

type marketplaceWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Owner       json.RawMessage `json:"owner"`
	Interface   *struct {
		DisplayName string `json:"displayName"`
	} `json:"interface"`
	Metadata *struct {
		Description string `json:"description"`
		Version     string `json:"version"`
		PluginRoot  string `json:"pluginRoot"`
	} `json:"metadata"`
	AllowCross []string           `json:"allowCrossMarketplaceDependenciesOn"`
	Renames    map[string]*string `json:"renames"`
	Plugins    []json.RawMessage  `json:"plugins"`
}

func parseMarketplaceFile(root, path string, raw []byte, ds *diagnostics) *Marketplace {
	var w marketplaceWire
	if err := json.Unmarshal(raw, &w); err != nil {
		ds.errorf("marketplace", path, RuleMarketplaceShape, "invalid JSON: %v", err)
		return &Marketplace{}
	}
	m := &Marketplace{Name: strings.TrimSpace(w.Name), Description: w.Description, Version: w.Version,
		Owner: parseAuthor(w.Owner, path, ds), AllowCrossMarketplaceDeps: w.AllowCross, Renames: w.Renames}
	if w.Interface != nil {
		m.DisplayName = w.Interface.DisplayName
	}
	if w.Metadata != nil {
		m.Description = firstNonEmpty(m.Description, w.Metadata.Description)
		m.Version = firstNonEmpty(m.Version, w.Metadata.Version)
		m.PluginRoot = w.Metadata.PluginRoot
	}
	pluginRoot := root
	if m.PluginRoot != "" {
		if p, ok := containedPath(root, m.PluginRoot); ok {
			pluginRoot = p
		} else {
			ds.errorf("marketplace", path, RuleMarketplaceShape, "metadata.pluginRoot %q must stay inside the marketplace", m.PluginRoot)
		}
	}
	seen := map[string]bool{}
	for i, item := range w.Plugins {
		e, ok := parseMarketplaceEntry(root, pluginRoot, path, i, item, ds)
		if !ok {
			continue
		}
		if seen[e.Name] {
			ds.errorf("marketplace", path, RuleMarketplaceEntry, "Duplicate plugin name %q found in marketplace", e.Name)
			continue
		}
		seen[e.Name] = true
		m.Plugins = append(m.Plugins, e)
	}
	return m
}

// entryWire 条目自身字段；其余 plugin.json 字段保留在 Manifest 原文中。
type entryWire struct {
	Name           string            `json:"name"`
	DisplayName    string            `json:"displayName"`
	Description    string            `json:"description"`
	Version        string            `json:"version"`
	Category       string            `json:"category"`
	Tags           []string          `json:"tags"`
	Keywords       []string          `json:"keywords"`
	Author         json.RawMessage   `json:"author"`
	Homepage       string            `json:"homepage"`
	Source         json.RawMessage   `json:"source"`
	Strict         *bool             `json:"strict"`
	DefaultEnabled *bool             `json:"defaultEnabled"`
	Dependencies   json.RawMessage   `json:"dependencies"`
	Policy         *CodexPolicy      `json:"policy"`
	Headers        map[string]string `json:"headers"`
	HeadersHelper  string            `json:"headersHelper"`
}

func parseMarketplaceEntry(root, pluginRoot, path string, i int, raw json.RawMessage, ds *diagnostics) (MarketplaceEntry, bool) {
	var w entryWire
	if err := json.Unmarshal(raw, &w); err != nil {
		ds.errorf("marketplace", path, RuleMarketplaceEntry, "plugins[%d]: invalid entry: %v", i, err)
		return MarketplaceEntry{}, false
	}
	if !validPluginName(w.Name) {
		ds.errorf("marketplace", path, RuleMarketplaceEntry, "plugins[%d]: plugin name %q is invalid", i, w.Name)
		return MarketplaceEntry{}, false
	}
	if w.HeadersHelper != "" {
		// headersHelper 是在用户机器上执行的命令，与 command 来源同属拒绝范围（决策七）。
		ds.errorf("marketplace", path, RuleMarketplaceSource, "plugins[%d] %s: headersHelper commands are not supported", i, w.Name)
		return MarketplaceEntry{}, false
	}
	src, err := parsePluginSource(root, pluginRoot, w.Source)
	if err != nil {
		ds.errorf("marketplace", path, RuleMarketplaceSource, "plugins[%d].source (%s): %v", i, w.Name, err)
		return MarketplaceEntry{}, false
	}
	e := MarketplaceEntry{Name: w.Name, DisplayName: w.DisplayName, Description: w.Description, Version: w.Version,
		Category: w.Category, Tags: w.Tags, Keywords: w.Keywords, Author: parseAuthor(w.Author, path, ds),
		Homepage: w.Homepage, Source: src, Strict: w.Strict == nil || *w.Strict, DefaultEnabled: w.DefaultEnabled,
		Policy: w.Policy, Headers: w.Headers, Manifest: raw}
	if len(w.Dependencies) > 0 {
		var items []json.RawMessage
		if json.Unmarshal(w.Dependencies, &items) != nil {
			ds.errorf("marketplace", path, RuleDependencyShape, "plugins[%d].dependencies must be an array", i)
		}
		for _, item := range items {
			if d, ok := parseDependency(item); ok {
				e.Dependencies = append(e.Dependencies, d)
			}
		}
	}
	if len(e.Headers) > 0 && src.Type != SourceArchive {
		ds.warnf("marketplace", path, RuleMarketplaceEntry, "Plugin %q sets headers, which only apply to \"archive\" sources", e.Name)
	}
	if src.Type == SourceArchive {
		e.Source.Headers = catalogHeaders(e.Name, w.Headers, path, ds)
	}
	return e, true
}

var (
	githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	sha1Pattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

type sourceWire struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	Repo     string `json:"repo"`
	URL      string `json:"url"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Package  string `json:"package"`
	Version  string `json:"version"`
	Registry string `json:"registry"`
	SHA256   string `json:"sha256"`
}

// parsePluginSource 两家来源：相对路径字符串、Codex {source:"local", path}，以及 github / url /
// git-subdir / npm / archive 对象。command 来源在用户机器上执行任意命令，拒绝（ADR-0103 决策七）。
func parsePluginSource(root, pluginRoot string, raw json.RawMessage) (PluginSource, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return relativeSource(root, pluginRoot, s)
	}
	var w sourceWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "Invalid input")
	}
	if w.Source == "local" {
		return relativeSource(root, pluginRoot, w.Path)
	}
	src := PluginSource{Type: w.Source, Repo: w.Repo, URL: w.URL, Ref: w.Ref, SHA: w.SHA, Package: w.Package,
		Version: w.Version, Registry: w.Registry, SHA256: w.SHA256}
	if w.Source == SourceGitSubdir {
		src.Path = strings.TrimPrefix(filepath.ToSlash(w.Path), "./")
	}
	if err := validateRemoteSource(w); err != nil {
		return PluginSource{}, err
	}
	return src, nil
}

// remoteSourceChecks 远程来源的字段约束（Claude marketplace reference「Plugin sources」）；返回问题描述。
func remoteSourceChecks() map[string]func(sourceWire) string {
	return map[string]func(sourceWire) string{
		SourceGitHub: func(w sourceWire) string {
			return unless(githubRepoPattern.MatchString(w.Repo), "github source requires repo in owner/repo form")
		},
		SourceURL: func(w sourceWire) string { return unless(w.URL != "", "url source requires url") },
		SourceGitSubdir: func(w sourceWire) string {
			return unless(w.URL != "" && w.Path != "" && !strings.Contains(w.Path, ".."),
				`git-subdir source requires url and a path without ".."`)
		},
		SourceNPM: func(w sourceWire) string {
			return unless(w.Package != "" && !strings.Contains(w.Package, ".."), `npm source requires package without ".."`)
		},
		SourceArchive: func(w sourceWire) string {
			return unless(strings.HasPrefix(w.URL, "https://") && (w.SHA256 == "" || sha256Pattern.MatchString(w.SHA256)),
				"archive source requires an https url and a 64-hex sha256")
		},
	}
}

func unless(ok bool, problem string) string {
	if ok {
		return ""
	}
	return problem
}

func validateRemoteSource(w sourceWire) error {
	if w.Source == "command" {
		return apperr.New(apperr.CodeForbidden, "command sources are not supported (they run arbitrary commands on the host)")
	}
	check, known := remoteSourceChecks()[w.Source]
	if !known {
		return apperr.New(apperr.CodeInvalidInput, "Invalid input: unknown source type "+w.Source)
	}
	problem := check(w)
	if problem == "" && w.SHA != "" && !sha1Pattern.MatchString(w.SHA) {
		problem = "sha must be a full 40-character lowercase commit SHA"
	}
	if problem != "" {
		return apperr.New(apperr.CodeInvalidInput, problem)
	}
	return nil
}

// relativeSource "./x" 相对市场根；"." 为根本身；裸名在 metadata.pluginRoot 下解析（Claude 规则）。
func relativeSource(root, pluginRoot, p string) (PluginSource, error) {
	switch {
	case p == ".":
		return PluginSource{Type: SourceRelative, Path: root}, nil
	case strings.HasPrefix(p, "./"):
		if strings.Contains(p[2:], `\`) {
			return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "path must use forward slashes")
		}
		abs, ok := containedPath(root, p)
		if !ok {
			return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "Path contains \"..\": "+p)
		}
		return PluginSource{Type: SourceRelative, Path: abs}, nil
	case p != "" && !strings.ContainsAny(p, `/\`) && p != ".." && pluginRoot != root:
		return PluginSource{Type: SourceRelative, Path: filepath.Join(pluginRoot, p)}, nil
	}
	return PluginSource{}, apperr.New(apperr.CodeInvalidInput, "Invalid input: relative source must start with ./")
}

func containedPath(root, rel string) (string, bool) {
	if strings.Contains(rel, "..") {
		return "", false
	}
	abs := filepath.Clean(filepath.Join(root, rel))
	r, err := filepath.Rel(root, abs)
	return abs, err == nil && !strings.HasPrefix(r, "..")
}

// mergeMarketplace Claude 文件先读：身份字段取先出现的非空值，Codex 文件补入 displayName、同名条目的
// policy / category，以及 Claude 文件中没有的条目。
func mergeMarketplace(dst, src *Marketplace) {
	dst.Name = firstNonEmpty(dst.Name, src.Name)
	dst.DisplayName = firstNonEmpty(dst.DisplayName, src.DisplayName)
	dst.Description = firstNonEmpty(dst.Description, src.Description)
	dst.Version = firstNonEmpty(dst.Version, src.Version)
	dst.PluginRoot = firstNonEmpty(dst.PluginRoot, src.PluginRoot)
	if dst.Owner == nil {
		dst.Owner = src.Owner
	}
	dst.AllowCrossMarketplaceDeps = append(dst.AllowCrossMarketplaceDeps, src.AllowCrossMarketplaceDeps...)
	if dst.Renames == nil {
		dst.Renames = src.Renames
	}
	index := map[string]int{}
	for i, e := range dst.Plugins {
		index[e.Name] = i
	}
	for _, e := range src.Plugins {
		if i, ok := index[e.Name]; ok {
			if dst.Plugins[i].Policy == nil {
				dst.Plugins[i].Policy = e.Policy
			}
			dst.Plugins[i].Category = firstNonEmpty(dst.Plugins[i].Category, e.Category)
			continue
		}
		dst.Plugins = append(dst.Plugins, e)
	}
}

// reservedMarketplaceNames Claude 官方/社区/目录保留名：仅来自 github.com/anthropics/ 的市场可用。
func reservedMarketplaceNames() map[string]bool {
	return map[string]bool{"claude-code-marketplace": true, "claude-code-plugins": true, "claude-plugins-official": true,
		"anthropic-marketplace": true, "anthropic-plugins": true, "agent-skills": true, "anthropic-agent-skills": true,
		"life-sciences": true, "knowledge-work-plugins": true, "claude-for-legal": true, "claude-for-financial-services": true,
		"financial-services-plugins": true, "first-party-plugins": true, "claude-tag-plugins": true, "claude-community": true,
		"claude-plugins-community": true, "healthcare": true, "anthropic-plugin-directory": true, "claude-plugin-directory": true}
}

// ReservedMarketplaceName 名称是否为保留名（含"另一种拼写"：尾随点、非下划线符号代替连字符）。
// fromAnthropics 为市场来源是否位于 github.com/anthropics/ 下。
func ReservedMarketplaceName(name string, fromAnthropics bool) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "npm", "pip", "uv", "cargo", "github", "gh", "inline", "builtin", "skills-dir", "synced", "claude-plugin-test":
		return true
	}
	if strings.HasPrefix(lower, "claudeai-") {
		return true
	}
	canonical := strings.TrimRight(regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(lower, "-"), "-")
	return !fromAnthropics && reservedMarketplaceNames()[canonical]
}

func validMarketplaceName(name string) bool {
	if name == "" || name == "." || strings.Contains(name, "..") || strings.ContainsAny(name, `/\ `) {
		return false
	}
	for _, r := range name {
		if r > 127 || r < 32 || isBidiControl(r) {
			return false // 非 ASCII 名称按冒充官方处理（Claude 规则）
		}
	}
	return true
}

// catalogHeaders 条目不得设置请求路由/身份类头（Claude：下载时丢弃并告警）。
func catalogHeaders(plugin string, in map[string]string, path string, ds *diagnostics) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		lk := strings.ToLower(k)
		switch {
		case lk == "host" || lk == "content-length" || lk == "transfer-encoding" || lk == "connection" || lk == "cookie" ||
			lk == "forwarded" || strings.HasPrefix(lk, "x-forwarded-") || strings.HasPrefix(lk, "proxy-"):
			ds.warnf("marketplace", path, RuleMarketplaceEntry,
				"Header %q is a request-routing/identity header that catalog entries may not set (plugin %s); dropped", k, plugin)
		default:
			out[k] = v
		}
	}
	return out
}
