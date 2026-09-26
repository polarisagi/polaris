package catalog

import (
	"encoding/json"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

// skillListingMaxRunes Claude：description 与 when_to_use 合计在技能列表中截断到 1536 字符。
const skillListingMaxRunes = 1536

// ToolView 技能以模型工具形态暴露时的名称、描述与参数 schema。
type ToolView struct {
	Name        string // LLM 工具名 "skill__{slug}"
	Description string
	InputSchema map[string]any
}

// ModelToolView 技能 → 模型可见工具视图；模型不可调用（disable-model-invocation /
// allow_implicit_invocation=false）或已废弃的技能返回 ok=false，不进入模型可见列表。
// 启动期注册（cmd/polaris/skill_loader.go）与动态目录（SkillCatalog）共用此函数，保证两条暴露路径语义一致。
func ModelToolView(meta types.SkillMeta) (ToolView, bool) {
	slug, ok := strings.CutPrefix(meta.Name, types.SkillPrefix)
	if !ok || slug == "" || meta.Deprecated || meta.DisableModelInvocation {
		return ToolView{}, false
	}
	// 只读取暴露所需的两个字段（JSON 键与 extension/pluginspec.Skill 一致）；本包位于 L1，
	// 不依赖 L2 的解析器类型。
	var spec struct {
		WhenToUse    string `json:"when_to_use"`
		ArgumentHint string `json:"argument_hint"`
	}
	if meta.Spec != "" && meta.Spec != "{}" {
		if err := json.Unmarshal([]byte(meta.Spec), &spec); err != nil {
			slog.Warn("skill: corrupt spec, exposing without argument hint", "skill", meta.Name, "err", err)
		}
	}
	argDesc := "技能参数原文（可选）"
	if spec.ArgumentHint != "" {
		argDesc = "技能参数，格式：" + spec.ArgumentHint
	}
	return ToolView{
		Name:        "skill__" + slug,
		Description: listingDescription(meta, spec.WhenToUse),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"arguments": map[string]any{"type": "string", "description": argDesc},
			},
		},
	}, true
}

func listingDescription(meta types.SkillMeta, whenToUse string) string {
	desc := meta.Description
	if desc == "" {
		for _, c := range meta.Capabilities {
			if d, ok := strings.CutPrefix(c, "description:"); ok {
				desc = d
				break
			}
		}
	}
	if whenToUse != "" {
		desc = strings.TrimSpace(desc + " " + whenToUse)
	}
	if meta.DisplayName != "" {
		desc = "[" + meta.DisplayName + "] " + desc
	}
	if utf8.RuneCountInString(desc) > skillListingMaxRunes {
		desc = string([]rune(desc)[:skillListingMaxRunes-1]) + "…"
	}
	return desc
}
