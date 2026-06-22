package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// MakeToolSearchFn 让 Agent 在已注册工具集中按名称/描述关键词搜索。
// query 为空时返回全量列表；匹配不区分大小写。
// 用途：Agent 可在执行前动态发现可用工具，避免幻觉式调用不存在的工具。
func MakeToolSearchFn(toolReg *InMemoryToolRegistry) sandbox.InProcessFn {
	return func(_ context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query string `json:"query"` // 搜索词（空=返回全量）
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tool_search: invalid args", err)
		}

		all := toolReg.List()
		query := strings.ToLower(strings.TrimSpace(args.Query))

		type toolSummary struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		}

		results := make([]toolSummary, 0, len(all))
		for _, t := range all {
			if query == "" ||
				strings.Contains(strings.ToLower(t.Name), query) ||
				strings.Contains(strings.ToLower(t.Description), query) {
				results = append(results, toolSummary{
					Name:        t.Name,
					Description: t.Description,
					Source:      string(t.Source),
				})
			}
		}

		// 按名称排序保证输出确定性（toolReg.List 依赖 map 迭代序不稳定）
		sort.Slice(results, func(i, j int) bool {
			return results[i].Name < results[j].Name
		})

		return json.Marshal(map[string]any{
			"tools": results,
			"total": len(results),
		})
	}
}
