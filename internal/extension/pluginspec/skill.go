package pluginspec

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SkillKind 区分技能来源形态。Claude 的 commands/*.md 是技能的旧形态，统一按技能承载
// （ADR-0103 决策三），但保留 Kind 以便 UI 展示与命名规则差异（命令名来自文件名）。
type SkillKind string

const (
	SkillKindSkill   SkillKind = "skill"
	SkillKindCommand SkillKind = "command"
)

// 技能规范条款标识（agentskills.io 规范 + Claude / Codex 扩展）。
const (
	RuleSkillFrontmatter   = "skill.frontmatter"
	RuleSkillNameRequired  = "skill.name.valid"
	RuleSkillNameCharset   = "agentskills.name.charset"
	RuleSkillNameDir       = "agentskills.name.matches-dir"
	RuleSkillDescMissing   = "agentskills.description.required"
	RuleSkillDescLength    = "agentskills.description.max-1024"
	RuleSkillCompatLength  = "agentskills.compatibility.max-500"
	RuleSkillMetadataShape = "agentskills.metadata.string-map"
	RuleSkillBoolValue     = "claude.skill.bool-value"
	RuleSkillLegacyPolaris = "polaris.skill.metadata-only"
	RuleSkillOpenAIYAML    = "codex.skill.openai-yaml"
)

const (
	skillNameMaxRunes    = 64
	skillDescMaxRunes    = 1024
	skillCompatMaxRunes  = 500
	polarisMetadataPrefx = "polaris-"
)

// agentskillsNamePattern：小写字母数字与单连字符，首尾非连字符。
var agentskillsNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill 归一化技能：agentskills.io 字段 + Claude 扩展字段 + Codex agents/openai.yaml。
// Name 为技能自身名（插件内对外名由调用方加 "<plugin>:" 前缀，见 QualifiedName）。
type Skill struct {
	Kind SkillKind `json:"kind"`
	Dir  string    `json:"dir"`  // 技能目录（${CLAUDE_SKILL_DIR}）；命令文件为其所在目录
	File string    `json:"file"` // SKILL.md 或命令 .md 的绝对路径

	Name          string            `json:"name"`
	Description   string            `json:"description"`
	WhenToUse     string            `json:"when_to_use,omitempty"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`

	AllowedTools           []string `json:"allowed_tools,omitempty"`
	DisallowedTools        []string `json:"disallowed_tools,omitempty"`
	ArgumentHint           string   `json:"argument_hint,omitempty"`
	Arguments              []string `json:"arguments,omitempty"`
	DisableModelInvocation bool     `json:"disable_model_invocation"`
	UserInvocable          bool     `json:"user_invocable"`
	Model                  string   `json:"model,omitempty"`
	Effort                 string   `json:"effort,omitempty"`
	Context                string   `json:"context,omitempty"` // "fork" 时在子 Agent 中运行
	Agent                  string   `json:"agent,omitempty"`
	Background             *bool    `json:"background,omitempty"`
	Paths                  []string `json:"paths,omitempty"`
	Shell                  string   `json:"shell,omitempty"`
	Hooks                  any      `json:"hooks,omitempty"` // 技能级 hooks，结构同 hooks.json 的 "hooks" 值

	OpenAI *OpenAISkillConfig `json:"openai,omitempty"`

	Body string `json:"-"` // frontmatter 之后的 Markdown 正文
}

// ModelInvocable 模型能否自动调用：Claude disable-model-invocation 与
// Codex policy.allow_implicit_invocation=false 任一关闭即不可（ADR-0103 决策五）。
func (s *Skill) ModelInvocable() bool {
	if s.DisableModelInvocation {
		return false
	}
	if s.OpenAI != nil && s.OpenAI.Policy.AllowImplicitInvocation != nil && !*s.OpenAI.Policy.AllowImplicitInvocation {
		return false
	}
	return true
}

// QualifiedName 插件内技能对外名 "<plugin>:<name>"；name 已带本插件前缀时不重复（Claude 规则）。
func QualifiedName(pluginName, skillName string) string {
	if pluginName == "" || strings.HasPrefix(skillName, pluginName+":") {
		return skillName
	}
	return pluginName + ":" + skillName
}

// PolarisParam 读取 Polaris 私有技能参数（只来自 metadata 的 "polaris-" 前缀键，ADR-0103 决策五）。
func (s *Skill) PolarisParam(key string) string {
	return s.Metadata[polarisMetadataPrefx+key]
}

// ParseSkillDir 解析技能目录（含 SKILL.md）。返回 nil 表示存在硬错误，诊断中给出原因。
func ParseSkillDir(dir string) (*Skill, []Diagnostic) {
	var ds diagnostics
	file := filepath.Join(dir, "SKILL.md")
	skill := parseSkillMarkdown(file, dir, filepath.Base(dir), SkillKindSkill, &ds)
	if skill == nil {
		return nil, ds
	}
	if cfg, ok := parseOpenAISkillYAML(dir, &ds); ok {
		skill.OpenAI = cfg
	}
	return skill, ds
}

// parseSkillMarkdown 解析 SKILL.md 或命令 .md。defaultName 为缺省名（目录名或命令名）。
func parseSkillMarkdown(file, dir, defaultName string, kind SkillKind, ds *diagnostics) *Skill {
	raw, err := os.ReadFile(file)
	if err != nil {
		ds.errorf(string(kind), file, RuleSkillFrontmatter, "read failed: %v", err)
		return nil
	}
	fields, body, err := splitFrontmatter(raw)
	if err != nil {
		ds.errorf(string(kind), file, RuleSkillFrontmatter, "%v", err)
		return nil
	}
	if fields == nil {
		fields = map[string]any{}
	}
	s := &Skill{Kind: kind, Dir: dir, File: file, Body: body, UserInvocable: true}
	applyIdentityFields(s, fields, defaultName, ds)
	if !validSkillName(s.Name) {
		ds.errorf(string(kind), file, RuleSkillNameRequired,
			"name %q must be 1-%d chars without whitespace, path separators, '..' or control characters", s.Name, skillNameMaxRunes)
		return nil
	}
	applyInvocationFields(s, fields, ds)
	warnAgentskillsRules(s, kind, defaultName, ds)
	warnLegacyPolarisKeys(fields, kind, file, ds)
	return s
}

func applyIdentityFields(s *Skill, fields map[string]any, defaultName string, ds *diagnostics) {
	// 命令文件的名称恒取文件名，frontmatter name 不生效（Claude 规则）。
	if s.Kind != SkillKindCommand {
		s.Name, _ = fmString(fields, "name")
		s.Name = strings.TrimSpace(s.Name)
	}
	if s.Name == "" {
		s.Name = defaultName
	}
	s.Description, _ = fmString(fields, "description")
	s.WhenToUse, _ = fmString(fields, "when_to_use")
	s.License, _ = fmString(fields, "license")
	s.Compatibility, _ = fmString(fields, "compatibility")
	meta, ok := fmStringMap(fields, "metadata")
	s.Metadata = meta
	if !ok {
		ds.warnf(string(s.Kind), s.File, RuleSkillMetadataShape, "metadata should map string keys to string values")
	}
	if strings.TrimSpace(s.Description) == "" {
		// Claude：缺省 description 回落为正文首个非空行。
		s.Description = firstNonEmptyLine(s.Body)
		ds.warnf(string(s.Kind), s.File, RuleSkillDescMissing, "description missing; using first line of body")
	}
}

func applyInvocationFields(s *Skill, fields map[string]any, ds *diagnostics) {
	s.AllowedTools = fmList(fields, "allowed-tools", " ,")
	s.DisallowedTools = fmList(fields, "disallowed-tools", " ,")
	s.ArgumentHint, _ = fmString(fields, "argument-hint")
	s.Arguments = fmList(fields, "arguments", " ")
	s.Model, _ = fmString(fields, "model")
	s.Effort, _ = fmString(fields, "effort")
	s.Context, _ = fmString(fields, "context")
	s.Agent, _ = fmString(fields, "agent")
	s.Shell, _ = fmString(fields, "shell")
	s.Paths = fmList(fields, "paths", ",")
	s.Hooks = fields["hooks"]

	for key, dst := range map[string]*bool{
		"disable-model-invocation": &s.DisableModelInvocation,
		"user-invocable":           &s.UserInvocable,
	} {
		v, present, valid := fmBool(fields, key)
		if !valid {
			ds.warnf(string(s.Kind), s.File, RuleSkillBoolValue, "%s is not a boolean; default kept", key)
			continue
		}
		if present {
			*dst = v
		}
	}
	if v, present, valid := fmBool(fields, "background"); present && valid {
		s.Background = &v
	}
}

// warnAgentskillsRules 输出 agentskills.io 规范告警（Claude 宽容处理，故不阻断加载）。
func warnAgentskillsRules(s *Skill, kind SkillKind, dirName string, ds *diagnostics) {
	comp := string(kind)
	bare := s.Name
	if i := strings.LastIndex(bare, ":"); i >= 0 {
		bare = bare[i+1:]
	}
	if kind == SkillKindSkill {
		if !agentskillsNamePattern.MatchString(bare) {
			ds.warnf(comp, s.File, RuleSkillNameCharset, "name %q should use lowercase letters, digits and single hyphens", s.Name)
		}
		if bare != dirName {
			ds.warnf(comp, s.File, RuleSkillNameDir, "name %q should match directory name %q", s.Name, dirName)
		}
	}
	if utf8.RuneCountInString(s.Description) > skillDescMaxRunes {
		ds.warnf(comp, s.File, RuleSkillDescLength, "description exceeds %d characters", skillDescMaxRunes)
	}
	if utf8.RuneCountInString(s.Compatibility) > skillCompatMaxRunes {
		ds.warnf(comp, s.File, RuleSkillCompatLength, "compatibility exceeds %d characters", skillCompatMaxRunes)
	}
}

// warnLegacyPolarisKeys 旧版 Polaris 顶层私有键须迁到 metadata 的 "polaris-" 前缀，
// 保证产物通过 skills-ref validate。
func warnLegacyPolarisKeys(fields map[string]any, kind SkillKind, file string, ds *diagnostics) {
	for _, k := range []string{"exec_mode", "ambient_priority", "risk_level", "sandbox", "capability", "version", "tags"} {
		if _, ok := fields[k]; ok {
			ds.warnf(string(kind), file, RuleSkillLegacyPolaris,
				"top-level %q is not a standard field and is ignored; use metadata.%s%s", k, polarisMetadataPrefx, strings.ReplaceAll(k, "_", "-"))
		}
	}
}

func validSkillName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > skillNameMaxRunes || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' || isBidiControl(r) {
			return false
		}
	}
	return true
}

// isBidiControl 双向格式控制字符可让显示名与真实名不一致（Claude 同样拒绝）。
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

func firstNonEmptyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return ""
}
