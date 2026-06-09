package substrate

import (
	"fmt"
	"strings"
	"unicode"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── 内容层提示注入检测 ──────────────────────────────────────────────────────
//
// Taint 系统保证数据来源可溯（结构层），ScanInjectionPatterns 覆盖内容层：
// 高置信度注入模式在 SanitizeToSafe 时拦截，防止"受信源的恶意内容"绕过类型边界。
// Advisory-only 设计原则：命中时拒绝降级（返回 error），不静默吞掉。

// injectionPattern 描述一条注入检测规则。
type injectionPattern struct {
	pattern string
	desc    string
}

// knownInjectionPatterns 覆盖 OWASP LLM01 常见间接注入手法。
// 保持简短（无正则）：高置信度匹配优先于覆盖率，减少假阳性。
var knownInjectionPatterns = []injectionPattern{
	{"ignore previous instructions", "role override attempt"},
	{"ignore all previous", "role override attempt"},
	{"disregard previous", "role override attempt"},
	{"forget your instructions", "role override attempt"},
	{"you are now", "persona hijack attempt"},
	{"act as if you are", "persona hijack attempt"},
	{"pretend you are", "persona hijack attempt"},
	{"system:", "system-role injection"},
	{"<system>", "xml system tag injection"},
	{"[system]", "bracket system tag injection"},
	{"### instruction", "markdown instruction injection"},
	{"## new instruction", "markdown instruction injection"},
	{"[inst]", "instruction tag injection"},
	{"</s>", "token boundary injection"},
	{"<|im_start|>", "chatml boundary injection"},
	{"<|im_end|>", "chatml boundary injection"},
}

// ScanInjectionPatterns 扫描内容是否含高置信度提示注入特征。
// 返回 (true, description) 表示检测到注入；(false, "") 表示清洁。
// 扫描在 Unicode 归一化的小写文本上执行，防止大小写/空格变体绕过。
func ScanInjectionPatterns(content string) (bool, string) {
	// 归一化：折叠 Unicode 空白、转小写
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return unicode.ToLower(r)
	}, content)

	for _, p := range knownInjectionPatterns {
		if strings.Contains(normalized, p.pattern) {
			return true, p.desc
		}
	}
	return false, ""
}

// Sanitizer 提供将 TaintedString 降级的策略集合。
// 架构文档: docs/arch/M11-Policy-Safety-深度选型.md §2.5

// SanitizeBySchema 基于强 Schema（format/pattern/enum）校验后降级。
// 结果: data.Level = min(Level-1, TaintMedium)
// 如果 hasStrictSchema 为 false（即裸 string，无 enum/pattern 约束），则拒绝降级。
func SanitizeBySchema(ts TaintedString, hasStrictSchema bool) (TaintedString, error) {
	if !hasStrictSchema {
		// fail-closed: 无法提供足够的注入防御
		return ts, perrors.New(perrors.CodeInvalidInput, "policy: strict schema (format/pattern/enum) required for sanitization")
	}

	newLevel := ts.Source.OriginTaintLevel - 1
	if newLevel > protocol.TaintMedium {
		newLevel = protocol.TaintMedium
	}
	if newLevel < protocol.TaintNone {
		newLevel = protocol.TaintNone
	}

	ts.Source.OriginTaintLevel = newLevel
	return ts, nil
}

// SanitizeBySummarization 经 LLM 摘要后降级。
// 结果: data.Level = max(min(Level-1, TaintMedium), TaintMedium)
// 永远带有 TaintMedium 硬地板，因为 LLM 输出可能包含 prompt injection 衍生内容。
func SanitizeBySummarization(ts TaintedString) TaintedString {
	newLevel := ts.Source.OriginTaintLevel - 1
	if newLevel > protocol.TaintMedium {
		newLevel = protocol.TaintMedium
	}
	// 硬地板
	if newLevel < protocol.TaintMedium {
		newLevel = protocol.TaintMedium
	}

	ts.Source.OriginTaintLevel = newLevel
	return ts
}

// SanitizeByUserReview 经人类用户显式确认后转换。
// 结果: data.Level = TaintUserReviewed
func SanitizeByUserReview(ts TaintedString, reviewerID string) TaintedString {
	ts.Source.OriginTaintLevel = protocol.TaintUserReviewed
	ts.Source.Module = fmt.Sprintf("user_review:%s", reviewerID)
	return ts
}

// SanitizeToSafe 尝试将污点数据彻底清洗为 SafeString，以便注入 Instruction Slot。
//
// 两阶段防护：
//  1. 结构层：TaintLevel > TaintLow 且非 TaintUserReviewed → 拒绝。
//  2. 内容层：TaintLevel >= TaintMedium 时对内容执行注入模式扫描。
//     命中高置信度注入特征 → 拒绝降级（避免"受信源内嵌恶意指令"绕过类型边界）。
func SanitizeToSafe(ts TaintedString) (SafeString, error) {
	if ts.Source.OriginTaintLevel > protocol.TaintLow && ts.Source.OriginTaintLevel != protocol.TaintUserReviewed {
		return SafeString{}, perrors.New(perrors.CodeInternal,
			fmt.Sprintf("policy: cannot sanitize level %s to SafeString (requires <= TaintLow or TaintUserReviewed)",
				ts.Source.OriginTaintLevel))
	}

	// 内容层扫描：TaintMedium 及以上来源执行注入检测（TaintUserReviewed 跳过：人类已显式审查）
	if ts.Source.OriginTaintLevel >= protocol.TaintMedium && ts.Source.OriginTaintLevel != protocol.TaintUserReviewed {
		if found, desc := ScanInjectionPatterns(ts.content); found {
			return SafeString{}, perrors.New(perrors.CodeInvalidInput,
				fmt.Sprintf("policy: injection pattern detected (%s) — sanitize blocked; "+
					"use SanitizeBySummarization or SanitizeByUserReview first", desc))
		}
	}

	return SafeString{content: ts.content}, nil
}
