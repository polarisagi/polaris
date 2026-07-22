package util

import "strings"

// extractJSON 从 LLM 响应中提取第一个 JSON 对象。
// LLM 有时在 JSON 外包裹 markdown 代码块，此函数做容错处理。
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去除 markdown 代码块包裹
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = s[idx:]
	}
	if idx := strings.LastIndex(s, "}"); idx >= 0 {
		s = s[:idx+1]
	}
	if len(s) == 0 {
		return "{}"
	}
	return s
}
