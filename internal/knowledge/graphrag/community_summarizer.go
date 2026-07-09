package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
)

// CommunitySummary Leiden 社区的自然语言摘要。
type CommunitySummary struct {
	CommunityID int      // 社区 ID（LeidenDetector 输出）
	NodeIDs     []string // 社区内节点 ID 列表
	Summary     string   // LLM 生成的自然语言摘要
	Keywords    []string // 主题关键词（从 Summary 提取）
}

// CommunityGenerativeSummarizer 将 Leiden 社区转化为自然语言摘要（M10 §2.7）。
// FeatureGraphRAGFull 门控（Tier 0+，≥8GB），<8GB VPS 时跳过 LLM 摘要生成。
type CommunityGenerativeSummarizer struct {
	provider protocol.Provider // LLM 生成摘要，必须注入
	maxNodes int               // 每社区最多采样节点数（防止 prompt 过长），默认 20
}

// NewCommunityGenerativeSummarizer 构造摘要生成器。provider 必须非 nil。
func NewCommunityGenerativeSummarizer(provider protocol.Provider) *CommunityGenerativeSummarizer {
	return &CommunityGenerativeSummarizer{
		provider: provider,
		maxNodes: 20,
	}
}

// Summarize 为每个社区生成自然语言摘要。
// communities: communityID → nodeContent 列表（由 Clusterer 提供）。
// 单社区 LLM 失败不阻断整体（best-effort）。
func (s *CommunityGenerativeSummarizer) Summarize(ctx context.Context, communities map[int][]string) ([]CommunitySummary, error) {
	if s.provider == nil || len(communities) == 0 {
		return nil, nil
	}

	results := make([]CommunitySummary, 0, len(communities))
	for cid, nodes := range communities {
		// 采样截断（防止 prompt token 超限）
		sampled := nodes
		if len(sampled) > s.maxNodes {
			sampled = sampled[:s.maxNodes]
		}

		prompt := fmt.Sprintf(
			"以下是一个知识图谱社区中的节点内容（%d 条）。\n"+
				"请用 2-3 句话总结该社区的核心主题，并列出 3-5 个关键词。\n"+
				"只回答 JSON，格式：{\"summary\":\"...\",\"keywords\":[\"...\",\"...\"]}\n\n"+
				"节点内容：\n%s",
			len(sampled),
			strings.Join(sampled, "\n---\n"),
		)

		//nolint:bare-infer // 历史代码暂留，后续重构替换
		resp, err := s.provider.Infer(ctx,
			[]types.Message{{Role: "user", Content: prompt}},
			types.WithMaxTokens(512),
		)
		if err != nil {
			// 单社区失败：跳过，不中断其他社区
			results = append(results, CommunitySummary{
				CommunityID: cid,
				NodeIDs:     nodes,
				Summary:     "[summary generation failed]",
			})
			continue
		}

		var out struct {
			Summary  string   `json:"summary"`
			Keywords []string `json:"keywords"`
		}
		// 解析失败退化为原始响应截断
		if parseErr := parseJSON(resp.Content, &out); parseErr != nil || out.Summary == "" {
			out.Summary = truncStr(resp.Content, 200)
		}

		results = append(results, CommunitySummary{
			CommunityID: cid,
			NodeIDs:     nodes,
			Summary:     out.Summary,
			Keywords:    out.Keywords,
		})
	}
	return results, nil
}

// parseJSON 从 LLM 响应提取并解析 JSON（容错 markdown 包裹）。
func parseJSON(s string, v any) error {
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = s[idx:]
	}
	if idx := strings.LastIndex(s, "}"); idx >= 0 {
		s = s[:idx+1]
	}
	return json.Unmarshal([]byte(s), v)
}

func truncStr(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
