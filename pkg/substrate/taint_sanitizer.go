package substrate

import (
	"fmt"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

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
// 只有当 TaintLevel <= TaintLow 时，才被视为对 Instruction Slot 安全。
func SanitizeToSafe(ts TaintedString) (SafeString, error) {
	if ts.Source.OriginTaintLevel > protocol.TaintLow && ts.Source.OriginTaintLevel != protocol.TaintUserReviewed {
		return SafeString{}, perrors.New(perrors.CodeInternal, fmt.Sprintf("policy: cannot sanitize level %s to SafeString (requires <= TaintLow or TaintUserReviewed)", ts.Source.OriginTaintLevel))
	}
	return SafeString{content: ts.content}, nil
}
