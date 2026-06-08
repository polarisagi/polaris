package knowledge

import (
	"sort"
	"strings"
	"time"
)

// KnowledgeConflictArbiter 知识冲突三级仲裁器。
// 架构文档: docs/arch/M10-Knowledge-RAG.md §4.2
//
// 当多来源知识存在冲突时，按三级策略仲裁:
//  1. AuthorityTier: 来源权威层级（官方文档 > 书籍 > 博客 > 未知）
//  2. Recency:       更新时间更近的优先（时间戳对比）
//  3. Consensus:     多来源支持的内容优先（多数票）
type KnowledgeConflictArbiter struct{}

// NewKnowledgeConflictArbiter 创建仲裁器。
func NewKnowledgeConflictArbiter() *KnowledgeConflictArbiter {
	return &KnowledgeConflictArbiter{}
}

// ConflictCandidate 冲突候选项。
type ConflictCandidate struct {
	Content    string    // 候选内容
	SourceType string    // 来源类型（决定 AuthorityTier）
	SourceURI  string    // 来源 URI
	UpdatedAt  time.Time // 最后更新时间
}

// authorityTier 返回来源类型的权威层级（越高越权威）。
// 分级规则对应文档 §4.2 AuthorityTier 定义。
func authorityTier(sourceType string) int {
	st := strings.ToLower(sourceType)
	switch {
	case strings.Contains(st, "official") || strings.Contains(st, "spec") || st == "kb_api":
		return 4 // 官方文档/API 规范
	case strings.Contains(st, "book") || st == "kb_doc":
		return 3 // 书籍/结构化文档
	case strings.Contains(st, "blog") || st == "kb_web":
		return 2 // 博客/网页
	case st == "episodic" || st == "semantic":
		return 1 // 内部记忆（次低权威）
	default:
		return 0 // 未知来源
	}
}

// Arbitrate 从冲突候选列表中选出最权威的一项。
// 返回选中的 ConflictCandidate 和仲裁依据。
// 候选列表为空时返回 nil。
func (a *KnowledgeConflictArbiter) Arbitrate(candidates []ConflictCandidate) (*ConflictCandidate, string) {
	if len(candidates) == 0 {
		return nil, "no candidates"
	}
	if len(candidates) == 1 {
		return &candidates[0], "single_candidate"
	}

	// 第一级：AuthorityTier 最高的候选（可能有多个）
	maxTier := -1
	for _, c := range candidates {
		if t := authorityTier(c.SourceType); t > maxTier {
			maxTier = t
		}
	}
	var topTier []ConflictCandidate
	for _, c := range candidates {
		if authorityTier(c.SourceType) == maxTier {
			topTier = append(topTier, c)
		}
	}
	if len(topTier) == 1 {
		return &topTier[0], "authority_tier"
	}

	// 第二级：时间最近的候选（可能有多个）
	sort.Slice(topTier, func(i, j int) bool {
		return topTier[i].UpdatedAt.After(topTier[j].UpdatedAt)
	})
	mostRecent := topTier[0].UpdatedAt
	var recentGroup []ConflictCandidate
	for _, c := range topTier {
		// 1 小时内视为同等时间
		if mostRecent.Sub(c.UpdatedAt) <= time.Hour {
			recentGroup = append(recentGroup, c)
		}
	}
	if len(recentGroup) == 1 {
		return &recentGroup[0], "recency"
	}

	// 第三级：多数共识——统计内容相似的候选归组，选最大组的代表
	best := consensusPick(recentGroup)
	return best, "consensus"
}

// ArbitrateChunks 对检索到的 Chunk 列表执行冲突检测与仲裁。
// 当两个 chunk 内容存在明显矛盾时触发仲裁。
// 返回去冲突后的 chunk 列表（保留权威版本，移除冲突的低权威版本）。
func (a *KnowledgeConflictArbiter) ArbitrateChunks(chunks []Chunk) []Chunk {
	if len(chunks) <= 1 {
		return chunks
	}

	// 按 SectionPath 归组，同 section 下的 chunk 相互比对
	groups := make(map[string][]Chunk)
	for _, c := range chunks {
		sectionKey := strings.Join(c.SectionPath, "/")
		groups[sectionKey] = append(groups[sectionKey], c)
	}

	var result []Chunk
	for _, group := range groups {
		if len(group) == 1 {
			result = append(result, group[0])
			continue
		}
		// 将同 section 的 chunk 转为 ConflictCandidate 仲裁
		var cands []ConflictCandidate
		for _, c := range group {
			cands = append(cands, ConflictCandidate{
				Content:    c.Content,
				SourceType: c.TaintSource,
				SourceURI:  c.DocID,
				UpdatedAt:  time.Now(), // chunk 无时间戳时以当前时间代替
			})
		}
		winner, _ := a.Arbitrate(cands)
		if winner != nil {
			// 找回对应的原 chunk
			for _, c := range group {
				if c.Content == winner.Content {
					result = append(result, c)
					break
				}
			}
		} else {
			result = append(result, group[0])
		}
	}
	return result
}

// consensusPick 从候选中选出内容相似度最高的多数组代表。
// 使用简单的字符串包含关系估计相似度（Tier 0 heuristic）。
func consensusPick(candidates []ConflictCandidate) *ConflictCandidate {
	if len(candidates) == 0 {
		return nil
	}
	maxSupport := 0
	var best *ConflictCandidate
	for i := range candidates {
		support := 0
		words := strings.Fields(strings.ToLower(candidates[i].Content))
		for j := range candidates {
			if i == j {
				continue
			}
			other := strings.ToLower(candidates[j].Content)
			// 关键词重叠超过 40% 视为支持
			overlap := 0
			for _, w := range words {
				if len(w) > 3 && strings.Contains(other, w) {
					overlap++
				}
			}
			if len(words) > 0 && float64(overlap)/float64(len(words)) > 0.4 {
				support++
			}
		}
		if support > maxSupport {
			maxSupport = support
			c := candidates[i]
			best = &c
		}
	}
	if best == nil {
		return &candidates[0]
	}
	return best
}
