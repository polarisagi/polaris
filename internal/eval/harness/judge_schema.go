package harness

import (
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// JudgeResult 是 LLM judge 返回的结构化评分结果。
type JudgeResult struct {
	Relevance    int    `json:"relevance"`
	Accuracy     int    `json:"accuracy"`
	Safety       int    `json:"safety"`
	Completeness int    `json:"completeness"`
	Passed       bool   `json:"passed"`
	Reason       string `json:"reason"`
}

// ValidateJudgeResultSchema 解析 LLM 返回的 JSON，校验所有必选字段是否存在。
// 返回 (result, schemaOK, parseErr)：
//   - schemaOK=false + parseErr=nil 表示 JSON 语法合法但 schema 缺字段。
//   - schemaOK=false + parseErr!=nil 表示 JSON 语法本身就错误。
func ValidateJudgeResultSchema(rawJSON string) (JudgeResult, bool, error) {
	requiredJudgeFields := []string{"relevance", "accuracy", "safety", "completeness", "passed", "reason"}

	// 第一步：先用 map 解析，检查必选 key 存在性（区分"schema缺字段"与"json语法错误"）
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		return JudgeResult{}, false, apperr.Wrap(apperr.CodeInternal, "judge schema: json syntax error", err)
	}
	for _, key := range requiredJudgeFields {
		if _, ok := raw[key]; !ok {
			slog.Warn("l4_judge: schema 缺字段", "missing_key", key)
			return JudgeResult{}, false, nil
		}
	}
	// 第二步：反序列化到具体结构体
	var result JudgeResult
	if err := json.Unmarshal([]byte(rawJSON), &result); err != nil {
		return JudgeResult{}, false, apperr.Wrap(apperr.CodeInternal, "judge schema: unmarshal error", err)
	}
	return result, true, nil
}
