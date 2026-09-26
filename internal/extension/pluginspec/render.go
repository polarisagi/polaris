package pluginspec

import (
	"regexp"
	"strconv"
	"strings"
)

// RenderInput 技能渲染上下文（Claude 技能字符串替换规则，ADR-0103 决策五）。
type RenderInput struct {
	RawArgs    string   // 调用参数原文（$ARGUMENTS）
	ArgNames   []string // frontmatter arguments：按位置映射 $name
	SkillDir   string   // ${CLAUDE_SKILL_DIR}
	PluginRoot string   // ${CLAUDE_PLUGIN_ROOT}（仅插件技能）
	PluginData string   // ${CLAUDE_PLUGIN_DATA}（仅插件技能）
	SessionID  string   // ${CLAUDE_SESSION_ID}
	ProjectDir string   // ${CLAUDE_PROJECT_DIR}
	Effort     string   // ${CLAUDE_EFFORT}
	// UserConfig 非敏感 userConfig 值；SensitiveKeys 中的键渲染为占位符（Claude：技能正文不替换敏感值）。
	UserConfig    map[string]string
	SensitiveKeys map[string]bool
}

// renderToken：可选的前导反斜杠 + ${...} / $ARGUMENTS[N] / $ARGUMENTS / $N / $name。
var renderToken = regexp.MustCompile(`(\\*)(\$\{[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?\}|\$ARGUMENTS\[\d+\]|\$ARGUMENTS|\$\d+|\$[A-Za-z_][A-Za-z0-9_]*)`)

// RenderSkill 单遍替换：参数值与变量值按字面插入，不再二次扫描（插入的 "$1" 不会被展开）。
// 单个反斜杠转义其后的占位符（输出去掉该反斜杠）；两个及以上反斜杠原样保留并照常展开。
// 正文未消费任何参数占位符且有参数时，追加 "ARGUMENTS: <原文>"（Claude 规则）。
func RenderSkill(body string, in RenderInput) string {
	positional := splitShellArgs(in.RawArgs)
	consumed := false
	out := renderToken.ReplaceAllStringFunc(body, func(m string) string {
		sub := renderToken.FindStringSubmatch(m)
		slashes, tok := sub[1], sub[2]
		if len(slashes) == 1 {
			return tok
		}
		val, ok, isArg := in.resolve(tok, positional)
		if !ok {
			return m
		}
		if isArg {
			consumed = true
		}
		return slashes + val
	})
	if !consumed && strings.TrimSpace(in.RawArgs) != "" {
		out = strings.TrimRight(out, "\n") + "\n\nARGUMENTS: " + in.RawArgs + "\n"
	}
	return out
}

// resolve 返回 (值, 是否替换, 是否参数占位符)。未知占位符原样保留（如 shell 片段里的 $HOME）。
func (in RenderInput) resolve(tok string, positional []string) (string, bool, bool) {
	if strings.HasPrefix(tok, "${") {
		v, ok := in.variable(strings.TrimSuffix(strings.TrimPrefix(tok, "${"), "}"))
		return v, ok, false
	}
	name := strings.TrimPrefix(tok, "$")
	switch {
	case name == "ARGUMENTS":
		return in.RawArgs, true, true
	case strings.HasPrefix(name, "ARGUMENTS["):
		return positionalArg(strings.TrimSuffix(strings.TrimPrefix(name, "ARGUMENTS["), "]"), positional)
	case name[0] >= '0' && name[0] <= '9':
		return positionalArg(name, positional)
	}
	for i, n := range in.ArgNames {
		if n == name {
			if i < len(positional) {
				return positional[i], true, true
			}
			return "", true, true // 具名参数无对应值展开为空串
		}
	}
	return "", false, false
}

// positionalArg 索引越界时占位符保持不变（Claude 规则）。
func positionalArg(idx string, positional []string) (string, bool, bool) {
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || i >= len(positional) {
		return "", false, false
	}
	return positional[i], true, true
}

func (in RenderInput) variable(name string) (string, bool) {
	vars := map[string]string{
		"CLAUDE_SKILL_DIR": in.SkillDir, "CLAUDE_PLUGIN_ROOT": in.PluginRoot, "PLUGIN_ROOT": in.PluginRoot,
		"CLAUDE_PLUGIN_DATA": in.PluginData, "PLUGIN_DATA": in.PluginData, "CLAUDE_SESSION_ID": in.SessionID,
		"CLAUDE_PROJECT_DIR": in.ProjectDir, "CLAUDE_EFFORT": in.Effort,
	}
	if v, ok := vars[name]; ok {
		return v, v != ""
	}
	key, ok := strings.CutPrefix(name, "user_config.")
	if !ok {
		return "", false
	}
	if in.SensitiveKeys[key] {
		return "<sensitive:" + key + ">", true
	}
	v, found := in.UserConfig[key]
	return v, found
}

// splitShellArgs shell 风格切分：空白分隔，单/双引号包裹的片段为一个参数，反斜杠转义下一字符。
func splitShellArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	inArg, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}
