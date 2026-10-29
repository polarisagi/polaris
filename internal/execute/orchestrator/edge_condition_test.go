package orchestrator

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// TestEvalEdgeCondition_NilIsAlwaysTrue 验证 nil 条件（无条件边）恒真，向后兼容
// 既有 WorkflowEdgeSpec 静态依赖语义。
func TestEvalEdgeCondition_NilIsAlwaysTrue(t *testing.T) {
	if !evalEdgeCondition(nil, []byte(`{"anything":"goes"}`)) {
		t.Fatal("nil condition must always evaluate true")
	}
}

// TestEvalEdgeCondition_EqNe 回归验证 GD-8-001 初版算子（eq/ne）行为不变。
func TestEvalEdgeCondition_EqNe(t *testing.T) {
	payload := []byte(`{"verdict":"pass"}`)
	if !evalEdgeCondition(&protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "pass"}, payload) {
		t.Error("eq: expected true for matching value")
	}
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "fail"}, payload) {
		t.Error("eq: expected false for non-matching value")
	}
	if !evalEdgeCondition(&protocol.EdgeCondition{Field: "verdict", Op: protocol.CondNotEquals, Value: "fail"}, payload) {
		t.Error("ne: expected true when value differs")
	}
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "verdict", Op: protocol.CondNotEquals, Value: "pass"}, payload) {
		t.Error("ne: expected false when value matches")
	}
}

// TestEvalEdgeCondition_Numeric 验证 GD-14-002 复核扩展新增的数值比较算子
// （gt/lt/ge/le），覆盖边界相等场景与非数字 fail-closed 场景。
func TestEvalEdgeCondition_Numeric(t *testing.T) {
	payload := []byte(`{"score":42}`)

	cases := []struct {
		name string
		op   protocol.EdgeConditionOp
		val  string
		want bool
	}{
		{"gt true", protocol.CondGreaterThan, "10", true},
		{"gt false", protocol.CondGreaterThan, "100", false},
		{"lt true", protocol.CondLessThan, "100", true},
		{"lt false", protocol.CondLessThan, "10", false},
		{"ge equal", protocol.CondGreaterOrEqual, "42", true},
		{"ge false", protocol.CondGreaterOrEqual, "43", false},
		{"le equal", protocol.CondLessOrEqual, "42", true},
		{"le false", protocol.CondLessOrEqual, "41", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalEdgeCondition(&protocol.EdgeCondition{Field: "score", Op: tc.op, Value: tc.val}, payload)
			if got != tc.want {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestEvalEdgeCondition_Numeric_NonNumericFailsClosed 验证数值算子在字段值非数字时
// fail-closed（不 panic、不误判为真），与既有"字段缺失/JSON 解析失败 fail-closed"
// 原则一致。
func TestEvalEdgeCondition_Numeric_NonNumericFailsClosed(t *testing.T) {
	payload := []byte(`{"score":"not-a-number"}`)
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "score", Op: protocol.CondGreaterThan, Value: "10"}, payload) {
		t.Fatal("expected fail-closed (false) when field value is not numeric")
	}
}

// TestEvalEdgeCondition_Contains 验证子串匹配算子。
func TestEvalEdgeCondition_Contains(t *testing.T) {
	payload := []byte(`{"message":"task completed with warnings"}`)
	if !evalEdgeCondition(&protocol.EdgeCondition{Field: "message", Op: protocol.CondContains, Value: "warnings"}, payload) {
		t.Error("expected contains match")
	}
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "message", Op: protocol.CondContains, Value: "errors"}, payload) {
		t.Error("expected contains non-match")
	}
}

// TestEvalEdgeCondition_Exists 验证字段存在性算子：不比较值，仅判定 Field 是否出现
// 在上游输出 JSON 中。
func TestEvalEdgeCondition_Exists(t *testing.T) {
	payload := []byte(`{"retry_count":3}`)
	if !evalEdgeCondition(&protocol.EdgeCondition{Field: "retry_count", Op: protocol.CondExists}, payload) {
		t.Error("expected exists=true for present field")
	}
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "missing_field", Op: protocol.CondExists}, payload) {
		t.Error("expected exists=false for absent field")
	}
}

// TestEvalEdgeCondition_AndOr 验证 GD-14-002 复核扩展新增的结构化 And/Or 复合条件，
// 覆盖递归嵌套（And 内嵌 Or）。
func TestEvalEdgeCondition_AndOr(t *testing.T) {
	payload := []byte(`{"verdict":"pass","score":90,"retries":1}`)

	// And: 全部子条件为真才为真。
	andCond := &protocol.EdgeCondition{And: []*protocol.EdgeCondition{
		{Field: "verdict", Op: protocol.CondEquals, Value: "pass"},
		{Field: "score", Op: protocol.CondGreaterOrEqual, Value: "80"},
	}}
	if !evalEdgeCondition(andCond, payload) {
		t.Error("And: expected true when all sub-conditions hold")
	}
	andCondFail := &protocol.EdgeCondition{And: []*protocol.EdgeCondition{
		{Field: "verdict", Op: protocol.CondEquals, Value: "pass"},
		{Field: "score", Op: protocol.CondGreaterOrEqual, Value: "95"},
	}}
	if evalEdgeCondition(andCondFail, payload) {
		t.Error("And: expected false when one sub-condition fails")
	}

	// Or: 任一子条件为真即为真。
	orCond := &protocol.EdgeCondition{Or: []*protocol.EdgeCondition{
		{Field: "verdict", Op: protocol.CondEquals, Value: "fail"},
		{Field: "retries", Op: protocol.CondLessThan, Value: "5"},
	}}
	if !evalEdgeCondition(orCond, payload) {
		t.Error("Or: expected true when at least one sub-condition holds")
	}

	// 递归嵌套：And 内嵌 Or。
	nested := &protocol.EdgeCondition{And: []*protocol.EdgeCondition{
		{Field: "verdict", Op: protocol.CondEquals, Value: "pass"},
		{Or: []*protocol.EdgeCondition{
			{Field: "score", Op: protocol.CondGreaterOrEqual, Value: "999"},
			{Field: "retries", Op: protocol.CondEquals, Value: "1"},
		}},
	}}
	if !evalEdgeCondition(nested, payload) {
		t.Error("nested And/Or: expected true")
	}
}

// TestEvalEdgeCondition_MissingFieldAndBadJSON_FailClosed 验证字段缺失与非法 JSON
// 场景下 fail-closed（不误触发）。
func TestEvalEdgeCondition_MissingFieldAndBadJSON_FailClosed(t *testing.T) {
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "absent", Op: protocol.CondEquals, Value: "x"}, []byte(`{}`)) {
		t.Error("expected false when field is missing")
	}
	if evalEdgeCondition(&protocol.EdgeCondition{Field: "x", Op: protocol.CondEquals, Value: "y"}, []byte(`not json`)) {
		t.Error("expected false when upstream output is not valid JSON")
	}
}
