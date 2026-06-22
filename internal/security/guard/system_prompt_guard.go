package guard

import (
	"strings"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SystemPromptGuard 防系统提示词泄露（OWASP LLM07 - M11 §2.2）。
// 架构文档: docs/arch/M11-Policy-Safety.md §2.2
//
// 机制：对所有出站文本（用户终端回复 + write_network 流量）扫描系统提示池；
// 发现连续重合 ≥ tokenThreshold 个词（token 近似）的片段 → 擦除或拒绝。
// 使用朴素滑动窗口匹配（生产级 Aho-Corasick 可替换 addFragment 中的索引结构）。
const (
	spgDefaultTokenThreshold = 15 // 连续重合词数阈值
)

var (
	ErrPromptLeakage = apperr.New(apperr.CodeForbidden, "system_prompt_guard: system prompt fragment detected in output")
)

// SystemPromptGuard 是系统提示词防泄露防线。
// 调用方（M4 executeEffect / M7 write_network 工具钩子）在向外发送内容前调用 Scan。
type SystemPromptGuard struct {
	mu             sync.RWMutex
	fragments      []string // 已注册系统提示片段（按词分割缓存）
	tokenThreshold int
}

// NewSystemPromptGuard 创建 SystemPromptGuard；tokenThreshold=0 使用默认值 15。
func NewSystemPromptGuard(tokenThreshold int) *SystemPromptGuard {
	if tokenThreshold <= 0 {
		tokenThreshold = spgDefaultTokenThreshold
	}
	return &SystemPromptGuard{tokenThreshold: tokenThreshold}
}

// AddFragment 向系统提示池注册一段系统提示文本。
// 启动时由 M4 Agent.Run 将 systemPrompt 内容注册进来。
func (g *SystemPromptGuard) AddFragment(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	g.mu.Lock()
	g.fragments = append(g.fragments, text)
	g.mu.Unlock()
}

// Scan 扫描出站文本，若发现系统提示片段则擦除或返回 ErrPromptLeakage。
//   - redact=true：将匹配片段替换为 "[SYSTEM_REDACTED]" 后返回净化文本
//   - redact=false：发现则直接返回 ErrPromptLeakage，拒绝出站
func (g *SystemPromptGuard) Scan(output string, redact bool) (string, error) {
	g.mu.RLock()
	fragments := g.fragments
	g.mu.RUnlock()

	result := output
	for _, frag := range fragments {
		hit, cleaned := g.detectAndRedact(result, frag)
		if !hit {
			continue
		}
		if !redact {
			return "", ErrPromptLeakage
		}
		result = cleaned
	}
	return result, nil
}

// detectAndRedact 检测 haystack 中是否存在来自 needle 的连续 ≥ tokenThreshold 个词的子序列。
// 使用滑动窗口近似（真实词序连续匹配，非 Aho-Corasick 但已足够防御 verbatim 泄露）。
func (g *SystemPromptGuard) detectAndRedact(haystack, needle string) (found bool, cleaned string) {
	needleWords := strings.Fields(needle)
	if len(needleWords) < g.tokenThreshold {
		// fragment 本身 token 数不足阈值，跳过（避免短词误伤）
		return false, haystack
	}

	// 在 needle 中以滑动窗口取长度 tokenThreshold 的子序列，在 haystack 中检索
	for start := 0; start+g.tokenThreshold <= len(needleWords); start++ {
		window := strings.Join(needleWords[start:start+g.tokenThreshold], " ")
		if idx := strings.Index(haystack, window); idx >= 0 {
			cleaned = strings.ReplaceAll(haystack, window, "[SYSTEM_REDACTED]")
			return true, cleaned
		}
	}
	return false, haystack
}
