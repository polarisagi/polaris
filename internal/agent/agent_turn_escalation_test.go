package agent

import (
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ADR-0101 决策六：只有能力类失败计入升级。安全拒绝与瞬时故障换更贵的模型不改变结果。
func TestFailureKindClassification(t *testing.T) {
	cases := []struct {
		name string
		got  fsm.FailureKind
		want fsm.FailureKind
	}{
		{"L0 结构错误", validationFailureKind(&protocol.DAGValidationError{Layer: "L0", Reason: "cycle"}), fsm.FailurePlanInvalid},
		{"L1 策略拒绝", validationFailureKind(&protocol.DAGValidationError{Layer: "L1_policy", Reason: "deny"}), fsm.FailurePolicy},
		{"L3 看门狗拒绝", validationFailureKind(&protocol.DAGValidationError{Layer: "L3_llm", Reason: "deny"}), fsm.FailurePolicy},
		{"L1 污点包装错误", validationFailureKind(apperr.New(apperr.CodeInternal, "taint")), fsm.FailurePolicy},
		{"执行超时", executionFailureKind(apperr.New(apperr.CodeTimeout, "slow")), fsm.FailureTransient},
		{"网络不可用", executionFailureKind(apperr.New(apperr.CodeNetworkUnavailable, "down")), fsm.FailureTransient},
		{"限流", executionFailureKind(apperr.New(apperr.CodeResourceExhausted, "429")), fsm.FailureTransient},
		{"参数错误", executionFailureKind(apperr.New(apperr.CodeInvalidInput, "bad arg")), fsm.FailureToolError},
		{"对象不存在", executionFailureKind(apperr.New(apperr.CodeNotFound, "no file")), fsm.FailureToolError},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s：want %v, got %v", c.name, c.want, c.got)
		}
	}
}
