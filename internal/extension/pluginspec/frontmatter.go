package pluginspec

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// splitFrontmatter 拆出 Markdown 文件开头的 YAML frontmatter 与正文。
// 无 frontmatter 时返回 (nil, 全文, nil)——Claude 命令文件与 agent 文件允许无 frontmatter。
// 以 "---" 开头但找不到闭合行视为语法错误，而非"无 frontmatter"：静默当正文处理会让
// 作者以为字段生效。
func splitFrontmatter(content []byte) (map[string]any, string, error) {
	text := strings.TrimPrefix(string(content), "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") && text != "---" {
		return nil, text, nil
	}
	rest := strings.TrimPrefix(text, "---\n")
	end := findClosingFence(rest)
	if end < 0 {
		return nil, "", apperr.New(apperr.CodeInvalidInput, "frontmatter: missing closing '---'")
	}
	yamlPart := rest[:end]
	body := strings.TrimPrefix(rest[end:], "---")
	body = strings.TrimPrefix(body, "\n")

	fields := map[string]any{}
	if strings.TrimSpace(yamlPart) != "" {
		dec := yaml.NewDecoder(bytes.NewReader([]byte(yamlPart)))
		if err := dec.Decode(&fields); err != nil {
			return nil, "", apperr.Wrap(apperr.CodeInvalidInput, "frontmatter: invalid YAML", err)
		}
	}
	return fields, body, nil
}

func findClosingFence(rest string) int {
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		return 0
	}
	idx := strings.Index(rest, "\n---\n")
	if idx >= 0 {
		return idx + 1
	}
	if strings.HasSuffix(rest, "\n---") {
		return len(rest) - 3
	}
	return -1
}

// fmString 读取字符串字段；非字符串标量按 YAML 文本回写（数字版本号 1.0 这类常见写法）。
func fmString(fields map[string]any, key string) (string, bool) {
	v, ok := fields[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case int, int64, float64, bool:
		return yamlScalar(t), true
	default:
		return "", false
	}
}

func yamlScalar(v any) string {
	out, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fmBool 按 Claude 规则解析布尔：true/false 之外还接受 yes/no/on/off/1/0（不区分大小写）。
// YAML 1.2 下 yes/no 是字符串，必须显式处理。
func fmBool(fields map[string]any, key string) (value, present, valid bool) {
	v, ok := fields[key]
	if !ok || v == nil {
		return false, false, true
	}
	switch t := v.(type) {
	case bool:
		return t, true, true
	case int:
		return t != 0, true, t == 0 || t == 1
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "on", "1":
			return true, true, true
		case "false", "no", "off", "0":
			return false, true, true
		}
	}
	return false, true, false
}

// fmList 读取 "字符串或列表" 字段。字符串按分隔符切分：allowed-tools 用空格或逗号，
// paths 只用逗号（glob 本身可含空格）。
func fmList(fields map[string]any, key string, seps string) []string {
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		return splitAny(t, seps)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

func splitAny(s, seps string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fmStringMap 读取 string→string 映射；返回 ok=false 表示存在但不是合法映射
// （agentskills 规定 metadata 为 string→string，Claude 丢弃非映射值）。
func fmStringMap(fields map[string]any, key string) (map[string]string, bool) {
	v, present := fields[key]
	if !present || v == nil {
		return nil, true
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, false
	}
	out := make(map[string]string, len(m))
	allStrings := true
	for k, raw := range m {
		switch t := raw.(type) {
		case string:
			out[k] = t
		case int, int64, float64, bool:
			out[k] = yamlScalar(t)
			allStrings = false
		default:
			allStrings = false
		}
	}
	return out, allStrings
}
