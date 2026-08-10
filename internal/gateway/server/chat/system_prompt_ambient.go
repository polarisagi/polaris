package chat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/store/search"
)

// ============================================================================
// Ambient skills 相关性判定与文本注入（R7 拆分自 system_prompt.go）。
// InjectSystemPrompt 主入口见 system_prompt.go；扩展摘要见
// system_prompt_extensions.go。
// ============================================================================

// skillEmbedCacheMax 技能文本→向量缓存上限。容量估算：512 × 1536 维 × 4 字节
// ≈ 3MB，可接受。
// [A-03 Step4] 原声明于 sse.go（HandleAgentStream 瘦身迁移时随之搬到唯一真实
// 消费方所在文件，非新引入常量）。
const skillEmbedCacheMax = 512

// skillEmbedCacheTTL 单条缓存的存活时长。
//
// 2026-08-10（A-06 / P-5 合规）：此前只有容量上限、无 TTL，且超限时靠 map 迭代
// "随机"淘汰一条。两个问题：
//   - 随机淘汰会以同等概率踢掉热点条目，命中率随缓存填满而抖动；
//   - 无 TTL 意味着 Embedder 换模型（embed_model_version 变更）后，旧模型算出
//     的向量会一直留在缓存里参与余弦比较，直到被随机淘汰撞上为止——不同模型的
//     向量空间不可比，比出来的相似度是噪声。
//
// 改为 TTL + LRU 双控（A-06 明文要求）：过期优先淘汰，无过期项时淘汰最久未访问
// 的一条。1 小时对"技能文本基本不变、模型偶尔更换"的场景足够。
const skillEmbedCacheTTL = time.Hour

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
func (s *PromptAssemblyService) cachedSkillEmbed(e search.Embedder, name, desc, inst string) []float32 {
	key := skillTextKey(name, desc, inst)
	now := time.Now()

	// 读路径要更新 lastUsed（LRU 需要），因此取写锁而非读锁。缓存条目数以技能
	// 数量为界（≤512），锁内只做 map 查找与两个字段赋值，争用可忽略。
	s.skillEmbedCacheMu.Lock()
	if ent, ok := s.skillEmbedCache[key]; ok {
		if now.Sub(ent.storedAt) < skillEmbedCacheTTL {
			ent.lastUsed = now
			s.skillEmbedCacheMu.Unlock()
			return ent.vec
		}
		delete(s.skillEmbedCache, key) // 已过期，回落到重新计算
	}
	s.skillEmbedCacheMu.Unlock()

	text := name + " " + desc + " " + inst
	v := e.Embed(text)
	if v == nil {
		return nil
	}

	s.skillEmbedCacheMu.Lock()
	defer s.skillEmbedCacheMu.Unlock()
	if s.skillEmbedCache == nil {
		// 兜底：测试等场景可能绕过 NewChatHandler 直接摆 struct literal 构造，
		// map 字段为零值 nil——写入前必须初始化，否则 panic。
		s.skillEmbedCache = make(map[string]*skillEmbedEntry)
	}
	if len(s.skillEmbedCache) >= skillEmbedCacheMax {
		s.evictSkillEmbedLocked(now)
	}
	s.skillEmbedCache[key] = &skillEmbedEntry{vec: v, storedAt: now, lastUsed: now}
	return v
}

// evictSkillEmbedLocked 腾出一个槽位：优先清掉全部已过期条目；一条都没过期时
// 淘汰最久未访问的一条（LRU）。调用方须持有 skillEmbedCacheMu 写锁。
//
// 线性扫描而非维护链表：容量上限 512，扫描成本远低于为此引入一套侵入式链表的
// 复杂度，且只在缓存满时触发。
func (s *PromptAssemblyService) evictSkillEmbedLocked(now time.Time) {
	evicted := false
	for k, ent := range s.skillEmbedCache {
		if now.Sub(ent.storedAt) >= skillEmbedCacheTTL {
			delete(s.skillEmbedCache, k)
			evicted = true
		}
	}
	if evicted {
		return
	}

	oldestKey := ""
	var oldest time.Time
	for k, ent := range s.skillEmbedCache {
		if oldestKey == "" || ent.lastUsed.Before(oldest) {
			oldestKey, oldest = k, ent.lastUsed
		}
	}
	if oldestKey != "" {
		delete(s.skillEmbedCache, oldestKey)
	}
}

// isSkillRelevant 判断技能是否与用户查询相关。
// Tier 2（Embedder 可用）：余弦相似度 >= EmbedThreshold。
// Tier 1（降级）：词元重叠度 >= relevanceThreshold。
// 任何错误静默降级 Tier 1，不中断聊天主流程。
func (s *PromptAssemblyService) isSkillRelevant(queryVec []float32, query, name, desc, inst string) bool {
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
func (s *PromptAssemblyService) buildAmbientSkillsSection(ctx context.Context, userQuery string) string {
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
	fullTextBudget := s.AmbientMaxChars
	if fullTextBudget <= 0 {
		fullTextBudget = defaultAmbientMaxChars
	}

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
func (s *PromptAssemblyService) SetActivatedSystemPrompt(taskType, promptText string) {
	if taskType != "general" {
		return
	}
	s.ActivatedSystemPromptMu.Lock()
	s.ActivatedSystemPrompt = promptText
	s.ActivatedSystemPromptMu.Unlock()
}
