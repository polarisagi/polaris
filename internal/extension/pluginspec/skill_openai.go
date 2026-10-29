package pluginspec

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// OpenAISkillConfig 对应 Codex 技能目录下的 agents/openai.yaml：UI 元数据、调用策略、工具依赖。
type OpenAISkillConfig struct {
	Interface    OpenAISkillInterface    `yaml:"interface" json:"interface"`
	Policy       OpenAISkillPolicy       `yaml:"policy" json:"policy"`
	Dependencies OpenAISkillDependencies `yaml:"dependencies" json:"dependencies"`
}

type OpenAISkillInterface struct {
	DisplayName      string `yaml:"display_name" json:"display_name,omitempty"`
	ShortDescription string `yaml:"short_description" json:"short_description,omitempty"`
	IconSmall        string `yaml:"icon_small" json:"icon_small,omitempty"`
	IconLarge        string `yaml:"icon_large" json:"icon_large,omitempty"`
	BrandColor       string `yaml:"brand_color" json:"brand_color,omitempty"`
	DefaultPrompt    string `yaml:"default_prompt" json:"default_prompt,omitempty"`
}

type OpenAISkillPolicy struct {
	// 指针区分"未设置"（默认 true）与显式 false。
	AllowImplicitInvocation *bool `yaml:"allow_implicit_invocation" json:"allow_implicit_invocation,omitempty"`
}

type OpenAISkillDependencies struct {
	Tools []OpenAISkillToolDependency `yaml:"tools" json:"tools,omitempty"`
}

// OpenAISkillToolDependency 技能声明的工具依赖（当前 Codex 仅定义 type=mcp）。
type OpenAISkillToolDependency struct {
	Type        string `yaml:"type" json:"type"`
	Value       string `yaml:"value" json:"value"`
	Description string `yaml:"description" json:"description,omitempty"`
	Transport   string `yaml:"transport" json:"transport,omitempty"`
	URL         string `yaml:"url" json:"url,omitempty"`
}

// parseOpenAISkillYAML 读取 <skill>/agents/openai.yaml；不存在返回 ok=false，解析失败记告警
// （该文件只影响 UI 与调用策略，不应让技能本身不可用）。
func parseOpenAISkillYAML(dir string, ds *diagnostics) (*OpenAISkillConfig, bool) {
	path := filepath.Join(dir, "agents", "openai.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cfg OpenAISkillConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		ds.warnf(string(SkillKindSkill), path, RuleSkillOpenAIYAML, "invalid agents/openai.yaml ignored: %v", err)
		return nil, false
	}
	return &cfg, true
}
