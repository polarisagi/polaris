package fsm

import (
	"strings"
	"unicode"
)

// IntentWeight 本轮用户输入的信息量分级（ADR-0102 决策一）。
//
// 纯字符串判定、零 LLM、零 IO：它决定的是"要不要花钱让 LLM 理解这句话"，
// 若本身调 LLM 就失去意义。判定只收窄成本路径，从不放宽安全路径——
// 被判为 Phatic 的输入只会走不挂工具的 S_RESPOND，不会触发任何动作。
type IntentWeight int

const (
	// IntentFull 常规输入：走完整 Perceive（记忆召回 + LLM 结构化）。
	IntentFull IntentWeight = iota
	// IntentAck 短确认（好的/ok/同意）：语义完全取决于上一轮对话——可能是
	// "同意执行你提议的删除"，必须经 Perceive 结合历史消解，**不得**直答；
	// 但长期记忆召回对它没有信息增益，跳过召回只保留对话历史。
	IntentAck
	// IntentPhatic 寒暄/致谢/告别：不携带任务语义，跳过 Perceive LLM 直接回复。
	IntentPhatic
)

// phaticMaxRunes 超过此长度不再视为寒暄——"你好，帮我看下这个报错"必须走完整路径。
const phaticMaxRunes = 16

// isPhatic 归一化后的寒暄/致谢/告别全集。只收"整句就是它"的形态，
// 宁可漏判（多花一次 Perceive）也不误判（把任务当寒暄吞掉）。
// 用 switch 而非包级 map：internal/ 禁包级可变变量（gochecknoglobals）。
func isPhatic(norm string) bool {
	switch norm {
	case "你好", "您好", "嗨", "哈喽", "早", "早安", "早上好", "中午好", "下午好", "晚上好",
		"在吗", "在不在", "大家好",
		"hi", "hello", "hey", "yo", "morning", "goodmorning", "goodafternoon", "goodevening":
		return true
	case "谢谢", "多谢", "感谢", "谢了", "辛苦了", "谢谢你", "谢谢您", "非常感谢", "太感谢了",
		"thanks", "thankyou", "thx", "ty", "thanksalot", "thankyousomuch", "manythanks":
		return true
	case "再见", "拜拜", "晚安", "回头见",
		"bye", "byebye", "goodbye", "goodnight", "seeyou", "cya":
		return true
	}
	return false
}

// isAck 归一化后的短确认。语义依赖上一轮，见 IntentAck。
func isAck(norm string) bool {
	switch norm {
	case "好", "好的", "好滴", "行", "可以", "可以的", "同意", "嗯", "嗯嗯", "对", "对的",
		"是的", "没问题", "收到", "继续", "确认", "明白", "了解", "知道了",
		"ok", "okay", "okk", "k", "yes", "yep", "yeah", "sure", "gotit", "goahead",
		"proceed", "continue", "confirm", "agreed", "lgtm":
		return true
	}
	return false
}

// ClassifyIntentWeight 对原始用户输入做零成本信息量分级。
func ClassifyIntentWeight(raw string) IntentWeight {
	norm := normalizeShortIntent(raw)
	if norm == "" {
		return IntentFull
	}
	if isPhatic(norm) {
		return IntentPhatic
	}
	if isAck(norm) {
		return IntentAck
	}
	return IntentFull
}

// normalizeShortIntent 小写、剔除标点/空白/符号（含 emoji）与句末语气词；
// 超长直接返回空串（交给完整路径）。
func normalizeShortIntent(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len([]rune(s)) > phaticMaxRunes {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	// 句末语气词反复剥离："你好呀~" "好的呢" "谢谢啦"
	for {
		trimmed := strings.TrimRightFunc(out, isChineseModalParticle)
		if trimmed == out || trimmed == "" {
			break
		}
		out = trimmed
	}
	return out
}

func isChineseModalParticle(r rune) bool {
	switch r {
	case '呀', '啊', '哈', '呢', '啦', '哦', '噢', '喔', '吧', '嘛', '哇':
		return true
	}
	return false
}
