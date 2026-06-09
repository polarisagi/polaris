package memory

import (
	"strings"
)

// ============================================================================
// QueryClassifier — Tier-0 规则路由（M05 §4.3）
// ============================================================================
// 纯函数，无状态，无 LLM 调用。
// 中文关键词匹配：Tier-0 成本约束禁止此处调用 embedding/LLM。

// QueryType 检索意图分类。
type QueryType int

const (
	QueryTypeUnknown   QueryType = iota
	QueryTypeTemporal            // 时间相关（最近事件、历史、今天等）
	QueryTypeFactual             // 实体/定义查询（是什么、谁是）
	QueryTypeHowTo               // 过程性知识（如何做、步骤）
	QueryTypeReasoning           // 分析推理（为什么、比较、影响）
)

// String 返回可读标签，便于日志追踪。
func (qt QueryType) String() string {
	switch qt {
	case QueryTypeTemporal:
		return "temporal"
	case QueryTypeFactual:
		return "factual"
	case QueryTypeHowTo:
		return "how_to"
	case QueryTypeReasoning:
		return "reasoning"
	default:
		return "unknown"
	}
}

// temporal 关键词信号：时间副词 + 时间段名词
var temporalKeywords = []string{
	"最近", "今天", "昨天", "上次", "上一次", "以前", "之前", "历史",
	"过去", "曾经", "记得", "还记得", "之前说过", "上周", "上个月",
	"recently", "last time", "previously", "history", "remember",
}

// factual 关键词信号：定义、实体查询
var factualKeywords = []string{
	"是什么", "是谁", "什么是", "定义", "含义", "解释", "概念",
	"what is", "what are", "who is", "define", "definition",
}

// howTo 关键词信号：过程、步骤
var howToKeywords = []string{
	"如何", "怎么", "怎样", "步骤", "教我", "示例", "例子", "方法",
	"how to", "how do", "how can", "steps", "tutorial",
}

// reasoning 关键词信号：分析、推理
var reasoningKeywords = []string{
	"为什么", "原因", "分析", "比较", "区别", "优缺点", "影响", "评估",
	"why", "because", "analyze", "compare", "difference", "pros and cons",
}

// ClassifyQuery 对 query 文本执行 Tier-0 意图分类。
// 按优先级降序：temporal > factual > how_to > reasoning > unknown。
func ClassifyQuery(query string) QueryType {
	lower := strings.ToLower(query)

	if matchAny(lower, temporalKeywords) {
		return QueryTypeTemporal
	}
	if matchAny(lower, howToKeywords) {
		return QueryTypeHowTo
	}
	if matchAny(lower, factualKeywords) {
		return QueryTypeFactual
	}
	if matchAny(lower, reasoningKeywords) {
		return QueryTypeReasoning
	}
	return QueryTypeUnknown
}

func matchAny(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
