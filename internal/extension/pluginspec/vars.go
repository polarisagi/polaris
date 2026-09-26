package pluginspec

import (
	"regexp"
	"strings"
)

// Vars 插件组件运行期变量（ADR-0103 决策二「路径变量」）。
type Vars struct {
	PluginRoot string            // ${PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT}
	PluginData string            // ${PLUGIN_DATA} / ${CLAUDE_PLUGIN_DATA}
	ProjectDir string            // ${CLAUDE_PROJECT_DIR}
	UserConfig map[string]string // ${user_config.KEY}
	// HostEnv 允许展开的宿主环境变量（调用方只放入 sanitizeParentEnv 白名单内的键）。
	// 不在此表中的 ${VAR} 视为未定义：插件可借 ${OPENAI_API_KEY} 这类引用把宿主密钥
	// 注入自身进程或请求头（R1.15 / HE-7）。
	HostEnv map[string]string
}

// varPattern 匹配 ${NAME} 与 ${NAME:-default}；NAME 允许 user_config.KEY 形式。
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)(?::-([^}]*))?\}`)

// Expand 单次、非递归、字面替换（agent-plugins 1.0：展开结果不再二次扫描）。
// 返回未解析（无值且无默认值）的变量名，供调用方告警。
func (v Vars) Expand(s string) (string, []string) {
	var unresolved []string
	out := varPattern.ReplaceAllStringFunc(s, func(tok string) string {
		m := varPattern.FindStringSubmatch(tok)
		name, def := m[1], m[2]
		hasDefault := strings.Contains(tok, ":-")
		if val, ok := v.lookup(name); ok {
			return val
		}
		if hasDefault {
			return def
		}
		unresolved = append(unresolved, name)
		return ""
	})
	return out, unresolved
}

func (v Vars) lookup(name string) (string, bool) {
	switch name {
	case "PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT":
		return v.PluginRoot, v.PluginRoot != ""
	case "PLUGIN_DATA", "CLAUDE_PLUGIN_DATA":
		return v.PluginData, v.PluginData != ""
	case "CLAUDE_PROJECT_DIR":
		return v.ProjectDir, v.ProjectDir != ""
	}
	if key, ok := strings.CutPrefix(name, "user_config."); ok {
		val, found := v.UserConfig[key]
		return val, found
	}
	val, found := v.HostEnv[name]
	return val, found
}

// ExpandAll 展开字符串切片与映射，汇总未解析变量。
func (v Vars) ExpandAll(args []string, m map[string]string) ([]string, map[string]string, []string) {
	var unresolved []string
	outArgs := make([]string, len(args))
	for i, a := range args {
		var u []string
		outArgs[i], u = v.Expand(a)
		unresolved = append(unresolved, u...)
	}
	var outMap map[string]string
	if m != nil {
		outMap = make(map[string]string, len(m))
		for k, val := range m {
			var u []string
			outMap[k], u = v.Expand(val)
			unresolved = append(unresolved, u...)
		}
	}
	return outArgs, outMap, unresolved
}

// ProcessEnv 插件 stdio 子进程需导出的路径变量（两家与 agent-plugins 三组名称同时导出）。
func (v Vars) ProcessEnv() map[string]string {
	env := map[string]string{}
	if v.PluginRoot != "" {
		env["PLUGIN_ROOT"] = v.PluginRoot
		env["CLAUDE_PLUGIN_ROOT"] = v.PluginRoot
	}
	if v.PluginData != "" {
		env["PLUGIN_DATA"] = v.PluginData
		env["CLAUDE_PLUGIN_DATA"] = v.PluginData
	}
	return env
}
