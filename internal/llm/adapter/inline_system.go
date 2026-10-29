package adapter

import (
	"regexp"
	"strings"
)

// ============================================================================
// 非首部 system 消息内联（ADR-0105 决策一 / WP6）。
//
// 内核 prompt 五层：L0 稳定核(system) → L1 会话层(system) → L2 历史(user/assistant) →
// L3 阶段层(system) → L4 回合层(user)。Anthropic/Gemini 把 system 提到独立参数，若把 L3
// 也提上去，阶段模板就跑到历史之前：Anthropic 上 system 变化使 system+messages 缓存全部失效，
// 各阶段共享的 L2 历史前缀作废。开启内联后只有**开头连续**的 system 进入 system 参数，
// 其后的 system 消息原位转成 user 角色的 <system_instruction> 文本块，位置随账本走。
//
// 安全论证（为何把 system 文本放进 user 角色不构成提权通道）：
//  1. 内联块内容来自内核自身的 TaintNone 指令（阶段模板/日期/环境提示）；若其中嵌有不可信文本
//     （外部目录、角色档案），写入方已用 UNTRUSTED_DATA 围栏包裹（PromptBuilder.WriteExternalCatalog
//     等）。
//  2. 数据区（L2 历史/L4 回合）的不可信内容由 PromptBuilder.WriteHistoryMessage/WriteUserData
//     经 taint.Spotlighting 围栏，但 Spotlighting 仅对 TaintMedium 及以上生效——TaintLow/None
//     的 user 文本（如用户直接输入）与 tool_result 的 Parts 不经围栏。因此不能只信围栏：
//     适配器层对所有**非内联来源**的 user 角色文本无条件转义 <system_instruction> 标签字面
//     （全角尖括号），攻击者无法在合并块内伪造/提前闭合内核指令块。
//  3. 内联块自身内容同样转义标签字面：它可能携带被围栏的不可信片段，转义对纯内核文本零影响
//     （标签字面本就不出现），却把"围栏内伪造闭合标签"的路径也封死。
//
// 转义是确定性的（同输入同输出），不破坏缓存前缀字节稳定。
// ============================================================================

const (
	inlineSystemOpen  = "<system_instruction>\n"
	inlineSystemClose = "\n</system_instruction>"
)

// systemInstructionTagRe 匹配 <system_instruction / </system_instruction（忽略大小写与标签内空白变体）。
var systemInstructionTagRe = regexp.MustCompile(`(?i)<(/?\s*system_instruction)`)

// escapeSystemInstructionTag 把 <system_instruction> 系标签的左尖括号替换为全角＜，使其失去标签语义。
// 不含该字样时原样返回（常见路径零分配）。
func escapeSystemInstructionTag(s string) string {
	if !strings.Contains(strings.ToLower(s), "system_instruction") {
		return s
	}
	return systemInstructionTagRe.ReplaceAllString(s, "＜$1")
}

// wrapInlineSystem 把一条非首部 system 消息包成 <system_instruction> 块；内容为空白时返回 ""，
// 调用方应丢弃该消息（空文本块会被 Anthropic 拒绝）。
func wrapInlineSystem(content string) string {
	body := strings.TrimSpace(content)
	if body == "" {
		return ""
	}
	return inlineSystemOpen + escapeSystemInstructionTag(body) + inlineSystemClose
}

// stablePartition 把满足 first 的元素稳定前移（其余保持原相对顺序）。
// 用于合并 user 内容时让 tool_result / functionResponse 块排在最前——两家 API 都要求工具结果
// 紧随对应的 tool_use 轮次且位于该 user 轮内容之首，内联的 system 文本块只能落在其后。
func stablePartition[T any](xs []T, first func(T) bool) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if first(x) {
			out = append(out, x)
		}
	}
	if len(out) == 0 || len(out) == len(xs) {
		return xs
	}
	for _, x := range xs {
		if !first(x) {
			out = append(out, x)
		}
	}
	return out
}
