package hook

import (
	"path"
	"strings"
)

// filterIf 处理 Claude 的 "if" 字段（权限规则语法 "Tool(pattern)"，如 "Bash(git *)"）：
// 工具名须相等，pattern 以 glob 匹配工具的主参数（command / file_path / path / url）。
// 无 if 或非工具事件的处理器原样保留；语法无法解析时按不匹配处理（宁可不跑也不误跑）。
func filterIf(handlers []boundHandler, in Input) []boundHandler {
	out := handlers[:0]
	for _, bh := range handlers {
		if bh.Handler.If == "" || ifMatches(bh.Handler.If, in) {
			out = append(out, bh)
		}
	}
	return out
}

func ifMatches(rule string, in Input) bool {
	open := strings.Index(rule, "(")
	if open <= 0 || !strings.HasSuffix(rule, ")") {
		return strings.EqualFold(strings.TrimSpace(rule), in.ToolName)
	}
	if !strings.EqualFold(strings.TrimSpace(rule[:open]), in.ToolName) {
		return false
	}
	pattern := strings.TrimSpace(rule[open+1 : len(rule)-1])
	arg := primaryArg(in.ToolInput)
	if pattern == "" || pattern == "*" {
		return true
	}
	// "git *" 形式：前缀 + 通配；path.Match 不跨 "/"，命令场景用前缀比较更符合直觉。
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok && !strings.ContainsAny(prefix, "*?[") {
		return strings.HasPrefix(arg, prefix)
	}
	ok, err := path.Match(pattern, arg)
	return err == nil && ok
}

func primaryArg(in map[string]any) string {
	for _, k := range []string{"command", "cmd", "file_path", "path", "url", "input"} {
		if s, ok := in[k].(string); ok {
			return s
		}
	}
	return ""
}
