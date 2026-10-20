package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 步骤预览生成：把工具的原始入参/结果转成人能一眼读懂的摘要，而不是直接截断原始 JSON。
// 只认已知工具的已知字段；认不出结构时退回通用截断，绝不 panic、绝不丢掉"出错了"这个事实。

// argsKeyPriority 通用入参摘要优先取的字段（命中第一个非空字符串）。
var argsKeyPriority = []string{"query", "url", "command", "path", "file_path", "pattern", "prompt", "name", "task"} //nolint:gochecknoglobals // 只读

func argsPreview(tool string, raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return clipRunes(string(raw), argsPreviewMax)
	}
	for _, k := range argsKeyPriority {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return clipRunes(v, argsPreviewMax)
		}
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return clipRunes(string(compact), argsPreviewMax)
}

func looksLikeToolError(raw string) bool {
	t := strings.TrimSpace(raw)
	return strings.HasPrefix(t, "Error:") || strings.HasPrefix(t, "error:") || strings.HasPrefix(t, "错误")
}

func looksDenied(raw string) bool {
	l := strings.ToLower(raw)
	return strings.Contains(l, "denied") || strings.Contains(raw, "拒绝") || strings.Contains(l, "not approved") ||
		strings.Contains(l, "forbidden")
}

// previewToolResult 生成 ≤resultPreviewMax 字符的结果摘要。
func previewToolResult(tool, raw string, isErr bool) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	if isErr {
		return clipRunes(t, resultPreviewMax)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(t), &obj) != nil {
		return clipRunes(t, resultPreviewMax)
	}
	switch {
	case tool == "web_search" || hasKey(obj, "results") && hasKey(obj, "engine"):
		if s, ok := previewSearch(obj); ok {
			return s
		}
	case tool == "web_fetch" || hasKey(obj, "final_url"):
		if s, ok := previewFetch(obj); ok {
			return s
		}
	case hasKey(obj, "exit_code"):
		return previewCommand(obj)
	}
	compact, err := json.Marshal(obj)
	if err != nil {
		return clipRunes(t, resultPreviewMax)
	}
	return clipRunes(string(compact), resultPreviewMax)
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func previewSearch(obj map[string]any) (string, bool) {
	list, ok := obj["results"].([]any)
	if !ok {
		return "", false
	}
	if len(list) == 0 {
		return "0 条结果", true
	}
	var titles []string
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if title, _ := m["title"].(string); strings.TrimSpace(title) != "" && len(titles) < 5 {
			titles = append(titles, clipRunes(title, 40))
		}
	}
	s := fmt.Sprintf("%d 条结果", len(list))
	if len(titles) > 0 {
		s += "：" + strings.Join(titles, "、")
		if len(list) > len(titles) {
			s += "…"
		}
	}
	return clipRunes(s, resultPreviewMax), true
}

func previewFetch(obj map[string]any) (string, bool) {
	title, _ := obj["title"].(string)
	content, _ := obj["content"].(string)
	if title == "" && content == "" {
		return "", false
	}
	s := ""
	if t := strings.TrimSpace(title); t != "" {
		s = "《" + clipRunes(t, 80) + "》"
	} else if u, _ := obj["final_url"].(string); u != "" {
		s = clipRunes(u, 80)
	}
	if st, ok := obj["status"].(float64); ok {
		s += fmt.Sprintf(" · HTTP %d", int(st))
	}
	s += fmt.Sprintf(" · %d 字", utf8.RuneCountInString(content))
	if tr, _ := obj["truncated"].(bool); tr {
		s += "（分页截断）"
	}
	return clipRunes(s, resultPreviewMax), true
}

func previewCommand(obj map[string]any) string {
	code := -1
	if f, ok := obj["exit_code"].(float64); ok {
		code = int(f)
	}
	out, _ := obj["output"].(string)
	s := fmt.Sprintf("退出码 %d", code)
	if e, _ := obj["error"].(string); e != "" && code != 0 {
		s += " · " + clipRunes(e, 120)
	}
	if lines := lastLines(out, 5); lines != "" {
		s += "\n" + lines
	}
	return clipRunes(s, resultPreviewMax)
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\r\n \t")
	if s == "" {
		return ""
	}
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
