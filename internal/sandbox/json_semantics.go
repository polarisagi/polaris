package sandbox

import (
	"encoding/json"
	"math/big"
	"strings"
)

func equalJSONValues(left, right any) bool {
	switch leftValue := left.(type) {
	case nil:
		return right == nil
	case bool:
		rightValue, ok := right.(bool)
		return ok && leftValue == rightValue
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue
	case json.Number:
		rightValue, ok := right.(json.Number)
		return ok && equalJSONNumbers(string(leftValue), string(rightValue))
	case []any:
		return equalJSONArray(leftValue, right)
	case map[string]any:
		return equalJSONObject(leftValue, right)
	default:
		return false
	}
}

func equalJSONArray(left []any, right any) bool {
	rightArray, ok := right.([]any)
	if !ok || len(left) != len(rightArray) {
		return false
	}
	for index := range left {
		if !equalJSONValues(left[index], rightArray[index]) {
			return false
		}
	}
	return true
}

func equalJSONObject(left map[string]any, right any) bool {
	rightObject, ok := right.(map[string]any)
	if !ok || len(left) != len(rightObject) {
		return false
	}
	for key, leftItem := range left {
		rightItem, exists := rightObject[key]
		if !exists || !equalJSONValues(leftItem, rightItem) {
			return false
		}
	}
	return true
}

type normalizedJSONNumber struct {
	negative bool
	digits   string
	exponent string
}

func equalJSONNumbers(left, right string) bool {
	leftNumber, leftOK := normalizeJSONNumber(left)
	rightNumber, rightOK := normalizeJSONNumber(right)
	return leftOK && rightOK && leftNumber == rightNumber
}

// 指数只做文本长度级的大整数运算，避免恶意指数触发巨量零或幂展开。
func normalizeJSONNumber(number string) (normalizedJSONNumber, bool) {
	negative := strings.HasPrefix(number, "-")
	unsigned := strings.TrimPrefix(number, "-")
	mantissa, exponentText, hasExponent := strings.Cut(strings.ToLower(unsigned), "e")
	exponent := new(big.Int)
	if hasExponent {
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return normalizedJSONNumber{}, false
		}
	}

	integerPart, fractionPart, hasFraction := strings.Cut(mantissa, ".")
	digits := integerPart + fractionPart
	if !hasFraction {
		fractionPart = ""
	}
	decimalPlaces := new(big.Int).SetInt64(int64(len(fractionPart)))
	exponent.Sub(exponent, decimalPlaces)
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return normalizedJSONNumber{digits: "0", exponent: "0"}, true
	}
	trimmed := strings.TrimRight(digits, "0")
	trailingZeros := len(digits) - len(trimmed)
	exponent.Add(exponent, new(big.Int).SetInt64(int64(trailingZeros)))
	return normalizedJSONNumber{negative: negative, digits: trimmed, exponent: exponent.String()}, true
}
