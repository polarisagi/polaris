package chat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/store/search"
)

// ============================================================================
// Ambient skills 相关性判定与文本注入（R7 拆分自 system_prompt.go）。
// InjectSystemPrompt 主入口见 system_prompt.go；扩展摘要见
// system_prompt_extensions.go。
// ============================================================================

func relevanceScore(query string, name string, desc string, inst string) float64 {
	queryLower := strings.ToLower(query)
	targetText := strings.ToLower(name + " " + desc + " " + inst)

	queryTokens := strings.Fields(queryLower)
	if len(queryTokens) == 0 {
		return 0
	}

	matchCount := 0
	for _, tk := range queryTokens {
		if strings.Contains(targetText, tk) {
			matchCount++
		}
	}

	return float64(matchCount) / float64(len(queryTokens))
}

// skillTextKey 返回技能文本的缓存 key（sha256 hex）。
func skillTextKey(name, desc, inst string) string {
	h := sha256.Sum256([]byte(name + "\x00" + desc + "\x00" + inst))
	return fmt.Sprintf("%x", h)
}

// cachedSkillEmbed 从缓存读取或调用 Embedder 获取技能向量。
// 失败时返回 nil（调用方降级 Tier 1）。
func (s *ChatHandler) cachedSkillEmbed(e search.Embedder, name, desc, inst string) []float32 {
	key := skillTextKey(name, desc, inst)
	s.skillEmbedCacheMu.RLock()
	if v, ok := s.skillEmbedCache[key]; ok {
		s.skillEmbedCacheMu.RUnlock()
		return v
	}
	s.skillEmbedCacheMu.RUnlock()

	text := name + " " + desc + " " + inst
	v := e.Embed(text)
	if v != nil {
		s.skillEmbedCacheMu.Lock()
		if s.skillEmbedCache == nil {
			// 兜底：测试等场景可能绕过 NewChatHandler 直接摆 struct literal 构造，
			// map 字段为零值 nil——写入前必须初始化，否则 panic。
			s.skillEmbedCache = make(map[string][]float32)
		}
		// 超限时随机淘汰一条（技能数量有界，随机淘汰比 LRU 实现简单且效果相近）
		if len(s.skillEmbedCache) >= skillEmbedCacheMax {
			for k := range s.skillEmbedCache {
				delete(s.skillEmbedCache, k)
				break
			}
		}
		s.skillEmbedCache[key] = v
		s.skillEmbedCacheMu.Unlock()
	}
	return v
}

// isSkillRelevant 判断技能是否与用户查询相关。
// Tier 2（Embedder 可用）：余弦相似度 >= EmbedThreshold。
// Tier 1（降级）：词元重叠度 >= relevanceThreshold。
// 任何错误静默降级 Tier 1，不中断聊天主流程。
func (s *ChatHandler) isSkillRelevant(queryVec []float32, query, name, desc, inst string) bool {
	if s.Embedder == nil || queryVec == nil {
		return relevanceScore(query, name, desc, inst) >= relevanceThreshold
	}

	skillVec := s.cachedSkillEmbed(s.Embedder, name, desc, inst)
	if skillVec == nil {
		return relevanceScore(query, name, desc, inst) >= relevanceThreshold
	}

	threshold := s.EmbedThreshold
	if threshold == 0 {
		threshold = 0.60
	}
	return ffi.VecCosineF32(queryVec, skillVec) >= float32(threshold)
}

// buildAmbientSkillsSection 按 trust_tier 和 ambient_priority 注入 ambient skill instructions
func (s *ChatHandler) buildAmbientSkillsSection(ctx context.Context, userQuery string) string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, description, instructions, plugin_id, ambient_priority, trust_tier
         FROM skills
         WHERE exec_mode='ambient' AND deprecated=0
         ORDER BY trust_tier DESC,
                  CASE ambient_priority WHEN 'always' THEN 0 WHEN 'auto' THEN 1 ELSE 2 END ASC`)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var indexLines []string
	var fullTextParts []string
	fullTextBudget := maxFullTextChars

	var queryVec []float32
	if s.Embedder != nil {
		queryVec = s.Embedder.Embed(userQuery)
	}

	for rows.Next() {
		var name, desc, inst, pluginID, ambientPriority string
		var trustTier int
		if rows.Scan(&name, &desc, &inst, &pluginID, &ambientPriority, &trustTier) != nil {
			continue
		}

		mcpMark := ""
		if s.MCPMgr != nil && s.MCPMgr.IsPluginConnected(pluginID) {
			mcpMark = " [MCP: ✓]"
		} else if pluginID != "" {
			mcpMark = " [MCP: ✗]"
		}

		indexLine := "- " + name + ": " + desc + mcpMark
		indexLines = append(indexLines, indexLine)

		if ambientPriority == "index_only" {
			continue
		}

		if ambientPriority == "auto" {
			if !s.isSkillRelevant(queryVec, userQuery, name, desc, inst) {
				continue
			}
		}

		if fullTextBudget-len(inst) < 0 {
			slog.Warn("ambient skill budget exhausted, index-only fallback", "skill", name)
			continue
		}

		entry := "### " + name + "\n" + inst
		fullTextParts = append(fullTextParts, entry)
		fullTextBudget -= len(entry)
	}

	if len(indexLines) == 0 {
		return ""
	}

	res := "\n\n## Installed Skills\n" + strings.Join(indexLines, "\n")
	if len(fullTextParts) > 0 {
		res += "\n\n## Active Skill Context\n" + strings.Join(fullTextParts, "\n\n")
	}
	return res
}

// SetActivatedSystemPrompt 热更新 M9 激活的系统提示词（goroutine-safe）。
// 由 PromptVersionStore.OnActivate 回调触发，对 task_type='general' 的激活版本生效。
func (s *ChatHandler) SetActivatedSystemPrompt(taskType, promptText string) {
	if taskType != "general" {
		return
	}
	s.ActivatedSystemPromptMu.Lock()
	s.ActivatedSystemPrompt = promptText
	s.ActivatedSystemPromptMu.Unlock()
}
