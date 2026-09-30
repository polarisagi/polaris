package agentctx

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/pkg/types"
)

// 召回条目渲染：紧凑化 / 估算 / 去重辅助（ADR-0105 决策四）。融合与装入见 recall_fuse.go。全部是纯函数，同输入字节一致。

const (
	// sourceMaxChars RAG 来源 URI 的单独上限。
	sourceMaxChars = 80
	// minContainKeyRunes 参与"被历史包含"去重的最短正文。更短的条目（"ok"、单词）
	// 几乎必然是任何长文本的子串，按包含判重会误杀。
	minContainKeyRunes = 12
)

// recallHeader 各段标题。回合内 Perceive/Plan 共用同一套，使复用的召回文本对两个阶段措辞一致。
// 用函数而非包级变量：internal/ 禁全局可变变量。
func recallHeader(kind fsm.RecallKind) string {
	switch kind {
	case fsm.RecallReflection:
		return "Cross-Session Reflections (past experience for similar tasks):\n"
	case fsm.RecallEpisodic:
		return "Relevant Historical Episodic Memories:\n"
	case fsm.RecallSemantic:
		return "Semantic Memory (L2):\n"
	case fsm.RecallRAG:
		return "Knowledge Base (RAG):\n"
	case fsm.RecallProfile:
		return "## User Profile (Context)\n"
	}
	return ""
}

// episodicSummary 把 payload 压成单行摘要（未截断，截断由 newRecallItem 统一做）。
func episodicSummary(payload []byte) string {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(trimmed, &obj) == nil {
			// 情景事件 payload 里常见的描述性字段，按优先级取第一个非空字符串。
			// payload 并无统一 schema（工具输出、{"tool":..,"status":..}、纯文本都有），
			// 故按字段名探测，探测不到再整体压成单行。
			for _, k := range [...]string{"summary", "description", "content", "text", "message", "goal", "result", "output"} {
				if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var buf bytes.Buffer
		if json.Compact(&buf, trimmed) == nil {
			return buf.String()
		}
	}
	return string(trimmed)
}

// newRecallItem 生成一条召回：正文折叠空白成单行（换行会破坏"一条一行"的结构，
// 也是 payload 里的提示注入常见载体），整行受 maxChars 约束，前缀计入上限。
func newRecallItem(prefix, body string, maxChars int) fsm.RecallItem {
	body = collapseSpace(body)
	room := maxChars - runeCount(prefix)
	if room < 1 {
		room = 1
	}
	body = truncateRunes(body, room)
	return fsm.RecallItem{Text: "- " + prefix + body, Key: body}
}

// collapseSpace 折叠所有空白为单个空格并剔除非法 UTF-8（规范化，也是去重的比较基准）。
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
}

func runeCount(s string) int { return utf8.RuneCountInString(s) }

// truncateRunes 按 rune 截断到不超过 max 个字符（含省略号），不切断多字节字符。
func truncateRunes(s string, max int) string {
	if max < 1 || utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// scoreFilter 返回各命中是否保留：低于绝对下限 min_score 的丢弃；低于 ratio×最高分的丢弃
// （最高分命中恒过相对阈值）。L2 分是无界 BM25、RAG 分是检索融合分，量纲互不相同，
// 所以每个来源只与自身的最高分比。ADR-0105 决策十起 ratio 默认 0（关闭）：来源间取舍由 RRF 承担。
func scoreFilter(scores []float64, l recallLimits) []bool {
	top := 0.0
	for _, s := range scores {
		if s > top {
			top = s
		}
	}
	keep := make([]bool, len(scores))
	for i, s := range scores {
		keep[i] = (l.minScore <= 0 || s >= l.minScore) &&
			(l.minScoreRatio <= 0 || top <= 0 || s >= top*l.minScoreRatio)
	}
	return keep
}

// estimateTokens 无分词器依赖的 token 估算：CJK 与全角标点按 1 token/字符，其余按 3 字节/token
// 向上取整。仓库现有 compact.RoughTokens 是 len/4（按字节，中文 3 字节/字 → 0.75 token/字，
// 系统性低估）；internal/llm 的 tiktoken 分词器离线时还要回退到估算，且 agent 层不应依赖 llm 包。
// 预算用途宁可略高估：JSON/代码的 token 密度高于 3 字节/token 的自然语言。
func estimateTokens(s string) int {
	wide, other := 0, 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r),
			unicode.Is(unicode.Hangul, r), (r >= 0x3000 && r <= 0x303F), (r >= 0xFF00 && r <= 0xFFEF):
			wide++
		default:
			other += utf8.RuneLen(r)
		}
	}
	return wide + (other+2)/3
}

// historyBlob 把 L2 对话历史规范化拼成一个可做子串判定的文本。
func historyBlob(history []types.Message) string {
	var sb strings.Builder
	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(collapseSpace(m.Content))
		sb.WriteByte('\n')
	}
	return sb.String()
}
