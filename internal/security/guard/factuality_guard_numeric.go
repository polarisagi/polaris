package guard

import (
	"strings"
)

// ============================================================================
// L2 NumericalConsistency 数值一致性检查（R7 拆分自 factuality_guard.go）。
// FactualityGuard 主体（Verify/L1 citationCheck/L3 semanticJudge）见
// factuality_guard.go。
// ============================================================================

// numericalCheck L2 数值一致性检查。
//
// 检测规则：
//  1. 概率/百分比超 100%（如 "120% accuracy", "150% probability"）→ Fail
//  2. 年份合理性：content 中出现早于 1900 或晚于 2100 的年份 → Uncertain
//  3. 负百分比（如 "-30% success rate"）→ Uncertain（可能指降幅，但疑似错误）
//  4. 若 contextDoc 中有数值，与 content 中相同量纲数值相差 >2 倍 → Uncertain
func (fg *FactualityGuard) numericalCheck(content, contextDoc string) (FactualityVerdict, string) {
	lContent := strings.ToLower(content)
	nums := extractNumbers(lContent)

	if v, msg := fg.checkProbability(nums, lContent); v != FactualityPass {
		return v, msg
	}

	if v, msg := fg.checkYearReasonability(nums); v != FactualityPass {
		return v, msg
	}

	if v, msg := fg.checkContextRatio(nums, lContent, contextDoc); v != FactualityPass {
		return v, msg
	}

	return FactualityPass, ""
}

func (fg *FactualityGuard) checkProbability(nums []string, lContent string) (FactualityVerdict, string) {
	probabilityKeywords := []string{"accuracy", "probability", "confidence", "precision", "recall", "f1"}
	for _, num := range nums {
		if len(num) < 2 {
			continue
		}
		idx := strings.Index(lContent, num+"%")
		if idx < 0 {
			continue
		}
		start := max(0, idx-50)
		end := min(len(lContent), idx+len(num)+20)
		context50 := lContent[start:end]
		for _, kw := range probabilityKeywords {
			if strings.Contains(context50, kw) {
				val := parseSimpleInt(num)
				if val < 0 {
					return FactualityUncertain, "suspicious negative percentage: " + num + "%"
				}
				if val > 100 {
					return FactualityFail, "invalid " + kw + " value: " + num + "% (exceeds 100%)"
				}
			}
		}
	}
	return FactualityPass, ""
}

func (fg *FactualityGuard) checkYearReasonability(nums []string) (FactualityVerdict, string) {
	for _, num := range nums {
		if len(num) == 4 {
			year := parseSimpleInt(num)
			if year > 0 && (year < 1900 || year > 2100) {
				return FactualityUncertain, "suspicious year value: " + num
			}
		}
	}
	return FactualityPass, ""
}

func (fg *FactualityGuard) checkContextRatio(nums []string, lContent, contextDoc string) (FactualityVerdict, string) {
	contextNums := extractNumbers(strings.ToLower(contextDoc))
	if len(contextNums) == 0 {
		return FactualityPass, ""
	}
	for _, num := range nums {
		val := parseSimpleInt(num)
		if val == 0 {
			continue
		}
		idx := strings.Index(lContent, num)
		if idx < 0 {
			continue
		}
		after := strings.TrimSpace(lContent[idx+len(num):])
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		unit := strings.Trim(fields[0], ".,;:\"'()")
		if len(unit) <= 1 {
			continue
		}
		if errStr := fg.checkUnitInContext(val, num, unit, contextNums, strings.ToLower(contextDoc)); errStr != "" {
			return FactualityUncertain, errStr
		}
	}
	return FactualityPass, ""
}

func (fg *FactualityGuard) checkUnitInContext(val int, num, unit string, contextNums []string, lContext string) string {
	for _, ctxNum := range contextNums {
		ctxIdx := strings.Index(lContext, ctxNum)
		if ctxIdx < 0 {
			continue
		}
		ctxAfter := strings.TrimSpace(lContext[ctxIdx+len(ctxNum):])
		ctxFields := strings.Fields(ctxAfter)
		if len(ctxFields) == 0 {
			continue
		}
		ctxUnit := strings.Trim(ctxFields[0], ".,;:\"'()")
		if unit != ctxUnit {
			continue
		}
		ctxVal := parseSimpleInt(ctxNum)
		if ctxVal == 0 {
			continue
		}
		if (val > 0 && ctxVal < 0) || (val < 0 && ctxVal > 0) {
			continue
		}
		ratio := float64(val) / float64(ctxVal)
		if ratio > 2.0 || ratio < 0.5 {
			return "value " + num + " " + unit + " differs significantly from context value " + ctxNum + " " + ctxUnit
		}
	}
	return ""
}

// parseSimpleInt 轻量整数解析（支持负数，无 strconv 依赖）。
func parseSimpleInt(s string) int {
	if len(s) == 0 {
		return 0
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
		s = s[1:]
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n * sign
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
