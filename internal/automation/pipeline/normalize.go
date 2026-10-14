package pipeline

import (
	"encoding/json"
	"strings"
)

// defaultItemKeys 探测顶层数组条目的候选键列表（按优先级依次尝试）。
var defaultItemKeys = []string{"items", "results", "entries", "data"}

// Normalize 归一化工具或步骤产出的原始数据。
// 规则：
// 1. 若原始数据为合法 JSON，先尝试 itemsPath 指定的点路径或依次探测 items/results/entries/data 顶层数组；
// 2. 若顶层本身即为数组，直接映射为 Items；
// 3. 若未提取到数组或不是 JSON，将原始文本存入 Text；
// 4. 若整体序列化体积超过 maxBytes，从尾部丢弃 items 直至 ≤ maxBytes，并置 Truncated=true。
func Normalize(raw []byte, itemsPath string, maxBytes int) StepData {
	if len(raw) == 0 {
		return StepData{Items: make([]map[string]any, 0), Count: 0}
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// 非 JSON，直接作为纯文本
		text := string(raw)
		res := StepData{
			Items: make([]map[string]any, 0),
			Text:  text,
			Count: 0,
		}
		if maxBytes > 0 && len(text) > maxBytes {
			res.Text = text[:maxBytes]
			res.Truncated = true
		}
		return res
	}

	var items []map[string]any
	if itemsPath != "" {
		items = extractPathItems(parsed, itemsPath)
	} else {
		items = extractAutoItems(parsed)
	}

	res := StepData{
		Items: items,
		Count: len(items),
	}
	if len(items) == 0 {
		res.Items = make([]map[string]any, 0)
		res.Text = string(raw)
	}

	// 截断检查：若序列化超出 maxBytes，从尾部缩减 items
	if maxBytes > 0 {
		b, _ := json.Marshal(res)
		if len(b) > maxBytes {
			res.Truncated = true
			if len(res.Items) > 0 {
				for len(res.Items) > 0 {
					res.Items = res.Items[:len(res.Items)-1]
					res.Count = len(res.Items)
					curBytes, _ := json.Marshal(res)
					if len(curBytes) <= maxBytes {
						break
					}
				}
			} else if len(res.Text) > maxBytes {
				res.Text = res.Text[:maxBytes]
			}
		}
	}

	return res
}

func extractAutoItems(parsed any) []map[string]any {
	switch v := parsed.(type) {
	case []any:
		return toMapSlice(v)
	case map[string]any:
		for _, key := range defaultItemKeys {
			if arr, ok := v[key].([]any); ok {
				return toMapSlice(arr)
			}
		}
	}
	return nil
}

func extractPathItems(parsed any, path string) []map[string]any {
	parts := strings.Split(path, ".")
	curr := parsed
	for _, part := range parts {
		m, ok := curr.(map[string]any)
		if !ok {
			return nil
		}
		curr, ok = m[part]
		if !ok {
			return nil
		}
	}
	if arr, ok := curr.([]any); ok {
		return toMapSlice(arr)
	}
	return nil
}

func toMapSlice(arr []any) []map[string]any {
	res := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			res = append(res, m)
		} else {
			res = append(res, map[string]any{"value": item})
		}
	}
	return res
}
