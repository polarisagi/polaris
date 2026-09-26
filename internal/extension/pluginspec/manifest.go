package pluginspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ManifestFormat 标识插件包中出现的清单格式（ADR-0103 决策二优先级表的三行）。
type ManifestFormat string

const (
	FormatAgentPlugins ManifestFormat = "agent-plugins" // 根 plugin.json（agent-plugins.org 1.0）
	FormatClaude       ManifestFormat = "claude"        // .claude-plugin/plugin.json
	FormatCodex        ManifestFormat = "codex"         // .codex-plugin/plugin.json 或 extensions["com.openai"]
)

const (
	agentPluginsSchemaPrefix = "https://agent-plugins.org/schemas/"
	openAIExtensionKey       = "com.openai"
)

const (
	RuleManifestParse   = "manifest.parse"
	RuleManifestSchema  = "agent-plugins.schema"
	RuleManifestUnknown = "manifest.unknown-field"
	RuleManifestAuthor  = "manifest.author"
	RuleManifestName    = "manifest.name"
	RuleManifestOverlay = "codex.extensions-overlay"
)

// Author 两家共用的作者对象。
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// manifestWire 三种格式字段的并集；多态字段以 RawMessage 延迟解析。
// 未出现在此结构中的顶层键按"未知字段"剥离并告警（Claude 语义）。
type manifestWire struct {
	Schema         string                     `json:"$schema"`
	Name           string                     `json:"name"`
	DisplayName    string                     `json:"displayName"`
	Version        string                     `json:"version"`
	Description    string                     `json:"description"`
	Author         json.RawMessage            `json:"author"`
	Homepage       string                     `json:"homepage"`
	Repository     json.RawMessage            `json:"repository"`
	License        string                     `json:"license"`
	Keywords       []string                   `json:"keywords"`
	Metadata       json.RawMessage            `json:"metadata"`
	DefaultEnabled *bool                      `json:"defaultEnabled"`
	Dependencies   json.RawMessage            `json:"dependencies"`
	Settings       json.RawMessage            `json:"settings"`
	UserConfig     json.RawMessage            `json:"userConfig"`
	Channels       json.RawMessage            `json:"channels"`
	Skills         json.RawMessage            `json:"skills"`
	Commands       json.RawMessage            `json:"commands"`
	Agents         json.RawMessage            `json:"agents"`
	Hooks          json.RawMessage            `json:"hooks"`
	MCPServers     json.RawMessage            `json:"mcpServers"`
	LSPServers     json.RawMessage            `json:"lspServers"`
	OutputStyles   json.RawMessage            `json:"outputStyles"`
	Workflows      json.RawMessage            `json:"workflows"`
	Themes         json.RawMessage            `json:"themes"`
	Monitors       json.RawMessage            `json:"monitors"`
	Experimental   json.RawMessage            `json:"experimental"`
	Apps           json.RawMessage            `json:"apps"`
	Interface      *CodexInterface            `json:"interface"`
	Extensions     map[string]json.RawMessage `json:"extensions"`
}

func knownManifestKeys() map[string]bool {
	keys := []string{"$schema", "name", "displayName", "version", "description", "author", "homepage",
		"repository", "license", "keywords", "metadata", "defaultEnabled", "dependencies", "settings",
		"userConfig", "channels", "skills", "commands", "agents", "hooks", "mcpServers", "lspServers",
		"outputStyles", "workflows", "themes", "monitors", "experimental", "apps", "interface", "extensions"}
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// CodexInterface Codex 清单 interface 块（UI 展示元数据）。
type CodexInterface struct {
	DisplayName       string          `json:"displayName,omitempty"`
	ShortDescription  string          `json:"shortDescription,omitempty"`
	LongDescription   string          `json:"longDescription,omitempty"`
	DeveloperName     string          `json:"developerName,omitempty"`
	Category          string          `json:"category,omitempty"`
	Capabilities      []string        `json:"capabilities,omitempty"`
	WebsiteURL        string          `json:"websiteURL,omitempty"`
	PrivacyPolicyURL  string          `json:"privacyPolicyURL,omitempty"`
	TermsOfServiceURL string          `json:"termsOfServiceURL,omitempty"`
	DefaultPrompt     json.RawMessage `json:"defaultPrompt,omitempty"` // 字符串或字符串数组（≤3 项）
	BrandColor        string          `json:"brandColor,omitempty"`
	ComposerIcon      string          `json:"composerIcon,omitempty"`
	Logo              string          `json:"logo,omitempty"`
	LogoDark          string          `json:"logoDark,omitempty"`
	Screenshots       []string        `json:"screenshots,omitempty"`
}

// manifestDoc 一份已解码的清单及其来源。
type manifestDoc struct {
	format ManifestFormat
	path   string
	wire   manifestWire
}

// readManifests 按优先级读取三种清单；extensions["com.openai"] 为对象时整体替换
// .codex-plugin/plugin.json（OpenAI 规定：覆盖而非合并）。
func readManifests(root string, ds *diagnostics) []manifestDoc {
	var docs []manifestDoc
	if doc, ok := readManifestFile(root, "plugin.json", FormatAgentPlugins, ds); ok {
		checkAgentPluginsSchema(doc, ds)
		docs = append(docs, doc)
	}
	if doc, ok := readManifestFile(root, filepath.Join(".claude-plugin", "plugin.json"), FormatClaude, ds); ok {
		docs = append(docs, doc)
	}
	if overlay, ok := openAIOverlay(docs, ds); ok {
		docs = append(docs, overlay)
		if _, exists := existingFile(root, ".codex-plugin", "plugin.json"); exists {
			ds.warnf("manifest", filepath.Join(root, ".codex-plugin", "plugin.json"), RuleManifestOverlay,
				"ignored: extensions[\"com.openai\"] in plugin.json replaces the Codex overlay")
		}
	} else if doc, ok := readManifestFile(root, filepath.Join(".codex-plugin", "plugin.json"), FormatCodex, ds); ok {
		docs = append(docs, doc)
	}
	return docs
}

func readManifestFile(root, rel string, format ManifestFormat, ds *diagnostics) (manifestDoc, bool) {
	path, ok := existingFile(root, rel)
	if !ok {
		return manifestDoc{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		ds.errorf("manifest", path, RuleManifestParse, "read failed: %v", err)
		return manifestDoc{}, false
	}
	var w manifestWire
	if err := json.Unmarshal(raw, &w); err != nil {
		ds.errorf("manifest", path, RuleManifestParse, "invalid JSON: %v", err)
		return manifestDoc{}, false
	}
	warnUnknownTopLevel(raw, path, ds)
	return manifestDoc{format: format, path: path, wire: w}, true
}

func warnUnknownTopLevel(raw []byte, path string, ds *diagnostics) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return
	}
	known := knownManifestKeys()
	for k := range top {
		if !known[k] {
			ds.warnf("manifest", path, RuleManifestUnknown, "unrecognized top-level field %q stripped", k)
		}
	}
}

func checkAgentPluginsSchema(doc manifestDoc, ds *diagnostics) {
	if !strings.HasPrefix(doc.wire.Schema, agentPluginsSchemaPrefix) {
		ds.warnf("manifest", doc.path, RuleManifestSchema,
			"root plugin.json should declare $schema %s<version>/plugin.schema.json", agentPluginsSchemaPrefix)
	}
}

// openAIOverlay 从 agent-plugins 根清单的 extensions["com.openai"] 构造 Codex 覆盖层。
func openAIOverlay(docs []manifestDoc, ds *diagnostics) (manifestDoc, bool) {
	for _, d := range docs {
		if d.format != FormatAgentPlugins {
			continue
		}
		raw, ok := d.wire.Extensions[openAIExtensionKey]
		if !ok {
			return manifestDoc{}, false
		}
		var w manifestWire
		if err := json.Unmarshal(raw, &w); err != nil {
			// agent-plugins：非对象 extensions 值非致命。
			ds.warnf("manifest", d.path, RuleManifestOverlay, "extensions[%q] is not an object: %v", openAIExtensionKey, err)
			return manifestDoc{}, false
		}
		return manifestDoc{format: FormatCodex, path: d.path + "#extensions/" + openAIExtensionKey, wire: w}, true
	}
	return manifestDoc{}, false
}

// parseAuthor 接受对象（两家标准）；字符串按 npm "Name <email> (url)" 习惯只取名字并告警。
func parseAuthor(raw json.RawMessage, path string, ds *diagnostics) *Author {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var a Author
	if err := json.Unmarshal(raw, &a); err == nil {
		if strings.TrimSpace(a.Name) == "" {
			ds.warnf("manifest", path, RuleManifestAuthor, "author.name is required when author is set")
		}
		return &a
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		ds.warnf("manifest", path, RuleManifestAuthor, "author should be an object {name,email,url}")
		name := s
		if i := strings.IndexAny(s, "<("); i > 0 {
			name = s[:i]
		}
		return &Author{Name: strings.TrimSpace(name)}
	}
	ds.warnf("manifest", path, RuleManifestAuthor, "author has an unsupported shape")
	return nil
}

// parseRepository 接受字符串或 npm 风格 {"type":"git","url":"..."}。
func parseRepository(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
}
