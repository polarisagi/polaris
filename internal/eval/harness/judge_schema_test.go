package harness

import (
	"testing"
)

func TestValidateJudgeResultSchema_AllFields(t *testing.T) {
	raw := `{"relevance":4,"accuracy":5,"safety":3,"completeness":4,"passed":true,"reason":"looks good"}`
	result, ok, err := ValidateJudgeResultSchema(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected schemaOK=true, got false")
	}
	if !result.Passed || result.Reason != "looks good" || result.Safety != 3 {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestValidateJudgeResultSchema_MissingField(t *testing.T) {
	// 只有 reason，缺其余必选字段
	raw := `{"reason":"only reason here"}`
	_, ok, err := ValidateJudgeResultSchema(raw)
	if err != nil {
		t.Fatalf("expected nil error for missing-field case, got: %v", err)
	}
	if ok {
		t.Fatal("expected schemaOK=false when required fields missing")
	}
}

func TestValidateJudgeResultSchema_JsonSyntaxError(t *testing.T) {
	raw := `{invalid json`
	_, ok, err := ValidateJudgeResultSchema(raw)
	if err == nil {
		t.Fatal("expected error for invalid JSON syntax")
	}
	if ok {
		t.Fatal("expected schemaOK=false for syntax error")
	}
}

func TestValidateJudgeResultSchema_EmptyInput(t *testing.T) {
	// util.ExtractJSON 对空字符串返回 "{}"，此处直接测 ValidateJudgeResultSchema("{}") schema缺字段
	_, ok, err := ValidateJudgeResultSchema("{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected schemaOK=false for empty object")
	}
}
