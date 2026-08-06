package knowledge

import (
	"math"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// 向量降级路径的轻量 JSON/float 解析 + 三路 RRF 融合（R7 拆分自 retriever.go）。
// 核心检索逻辑（Search/searchFTS/searchVector/cosine 等）见 retriever.go。
// ============================================================================

// parseEmbedding 解析 JSON 格式的 float32 数组（[0.1,0.2,...]）。
func parseEmbedding(s string) ([]float32, error) {
	// 使用简单 JSON 解析
	var vals []float32
	// 手动解析: "[f,f,f,...]"
	if len(s) < 2 || s[0] != '[' {
		return nil, apperr.New(apperr.CodeInvalidInput, "invalid embedding format")
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return nil, nil
	}
	// 按逗号分割
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			token := strings.TrimSpace(s[start:i])
			if token != "" {
				vals = append(vals, float32(parseFloat(token)))
			}
			start = i + 1
		}
	}
	return vals, nil
}

// parseFloat 轻量 float 解析（无 strconv 依赖，Tier 0 Wasm 友好）。
func parseFloat(s string) float64 {
	i := 0
	neg := pfSign(s, &i)
	intPart := pfDigits(s, &i)
	fracPart := pfFrac(s, &i)
	exp := pfExp(s, &i)
	val := (intPart + fracPart) * math.Pow(10, exp)
	if neg {
		val = -val
	}
	return val
}

func pfSign(s string, i *int) bool {
	if *i < len(s) {
		switch s[*i] {
		case '-':
			*i++
			return true
		case '+':
			*i++
			return false
		}
	}
	return false
}

func pfDigits(s string, i *int) float64 {
	var v float64
	for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
		v = v*10 + float64(s[*i]-'0')
		*i++
	}
	return v
}

func pfFrac(s string, i *int) float64 {
	var v float64
	if *i < len(s) && s[*i] == '.' {
		*i++
		scale := 0.1
		for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
			v += float64(s[*i]-'0') * scale
			scale *= 0.1
			*i++
		}
	}
	return v
}

func pfExp(s string, i *int) float64 {
	if *i >= len(s) || (s[*i] != 'e' && s[*i] != 'E') {
		return 0
	}
	*i++
	neg := false
	if *i < len(s) && s[*i] == '-' {
		neg = true
		*i++
	} else if *i < len(s) && s[*i] == '+' {
		*i++
	}
	var exp float64
	for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
		exp = exp*10 + float64(s[*i]-'0')
		*i++
	}
	if neg {
		return -exp
	}
	return exp
}
