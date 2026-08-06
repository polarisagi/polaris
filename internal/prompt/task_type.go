package prompt

import "strings"

// ExtractTaskType 从任务目标字符串提取规范化任务类型键。
// 取前 3 个非空词的小写形式作为分组 key。
// 示例: "Write a Python function to sort..." → "write_a_python"
// MVP 降级方案：若 StateContext 未来新增 TaskType 字段，直接使用该字段替代。
func ExtractTaskType(goal string) string {
	words := strings.Fields(strings.ToLower(goal))
	if len(words) == 0 {
		return "unknown"
	}
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, "_")
}
