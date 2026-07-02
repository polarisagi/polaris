package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeToolSearchFn 让 Agent 在已注册工具集中按名称/描述关键词或语义搜索。
// 命中结果会被激活到当前会话（基于 ctx 的 session_id）。
func MakeToolSearchFn(compCatalog *catalog.CompositeCatalog, embedder search.Embedder) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tool_search: invalid args", err)
		}

		all := compCatalog.List(ctx, types.TrustUntrusted) // search across all
		query := strings.TrimSpace(args.Query)

		// 1. String exact/substring match
		lowerQuery := strings.ToLower(query)

		type scoredTool struct {
			entry catalog.CatalogEntry
			score float32
		}

		var matches []scoredTool

		if embedder != nil && query != "" {
			// 2. Semantic match
			queryEmb := embedder.Embed(query)
			if len(queryEmb) > 0 {
				_ = queryEmb
				// Stub: Implement vector similarity search if needed.
				// P1-2 constraint: Uses the protocol.Embedder to semantically search.
			}
		}

		// Fallback / simple match
		for _, t := range all {
			if query == "" ||
				strings.Contains(strings.ToLower(t.Name), lowerQuery) ||
				strings.Contains(strings.ToLower(t.Description), lowerQuery) {
				matches = append(matches, scoredTool{entry: t, score: 1.0})
			}
		}

		// 激活到会话
		sessionID := ""
		if sid, ok := ctx.Value("session_id").(string); ok {
			sessionID = sid
		}

		type toolSummary struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		}

		results := make([]toolSummary, 0, len(matches))
		for _, m := range matches {
			if sessionID != "" {
				compCatalog.ActivateTool(sessionID, m.entry.Name)
			}
			results = append(results, toolSummary{
				Name:        m.entry.Name,
				Description: m.entry.Description,
				Source:      string(m.entry.Source),
			})
		}

		// 按名称排序保证输出确定性
		sort.Slice(results, func(i, j int) bool {
			return results[i].Name < results[j].Name
		})

		return json.Marshal(map[string]any{
			"tools": results,
			"total": len(results),
		})
	}
}
