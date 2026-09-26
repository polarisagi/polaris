package lifecycle

import (
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/types"
)

// PluginSkillName 插件内技能的 skills 表主键："skill:{plugin}__{skill}"。
// 用 "__" 而非 Claude 的 ":" 分隔：LLM 工具名由 "skill__"+slug 构成，须满足 ^[a-zA-Z0-9_-]+$；
// 命令名中的 ":"（子目录层级）同样转为 "__"。对外展示名见 pluginspec.QualifiedName。
func PluginSkillName(pluginName, skillName string) string {
	return types.SkillPrefix + mcp.SanitizeToolNamePart(pluginName) + "__" + skillSlug(skillName)
}

// StandaloneSkillName 独立安装技能的主键："skill:{skill}"。
func StandaloneSkillName(skillName string) string {
	return types.SkillPrefix + skillSlug(skillName)
}

func skillSlug(name string) string {
	return mcp.SanitizeToolNamePart(strings.ReplaceAll(name, ":", "__"))
}

// skillMetaFromSpec 归一化技能 → skills 表元数据。Polaris 私有参数只取 metadata 的
// "polaris-" 前缀键（ADR-0103 决策五）；Instructions 为正文（frontmatter 是元数据，不进上下文）。
// displayName 为对外名（插件技能 "plugin:skill"）。
func skillMetaFromSpec(s *pluginspec.Skill, name, displayName, version, pluginID string, trust types.TrustTier) types.SkillMeta {
	caps := []string{"description:" + s.Description}
	if c := s.PolarisParam("capability"); c != "" {
		caps = append(caps, "capability:"+c)
	}
	spec, err := json.Marshal(s)
	if err != nil {
		spec = []byte("{}")
	}
	return types.SkillMeta{
		Name:                   name,
		Version:                firstNonEmpty(s.PolarisParam("version"), s.Metadata["version"], version, "1.0.0"),
		Runtime:                "script",
		RiskLevel:              firstNonEmpty(s.PolarisParam("risk-level"), "medium"),
		Sandbox:                sandboxLevel(s.PolarisParam("sandbox")),
		Capabilities:           caps,
		ExecMode:               firstNonEmpty(s.PolarisParam("exec-mode"), "tool"),
		AmbientPriority:        firstNonEmpty(s.PolarisParam("ambient-priority"), "auto"),
		Trust:                  trust,
		Instructions:           s.Body,
		PluginID:               pluginID,
		Description:            s.Description,
		DisplayName:            displayName,
		Kind:                   string(s.Kind),
		DisableModelInvocation: !s.ModelInvocable(),
		DisableUserInvocation:  !s.UserInvocable,
		SkillDir:               s.Dir,
		Spec:                   string(spec),
	}
}

// sandboxLevel 将 "L1"/"L2"/"L3" 映射为 skills.sandbox 整数；缺省 L1。
func sandboxLevel(s string) int {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "L3":
		return 3
	case "L2":
		return 2
	default:
		return 1
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
