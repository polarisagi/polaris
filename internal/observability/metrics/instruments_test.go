package metrics

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

// TestInstrumentInitErrs_Capture_S02 验证聚合器基本行为：attempts 累计全部调用，
// errs 只收集失败调用。阶段02 §2.2 新增，避免 initInstruments 内 30+ 处逐个吞没。
func TestInstrumentInitErrs_Capture_S02(t *testing.T) {
	ie := &instrumentInitErrs{}
	ie.capture("a", nil)
	ie.capture("b", errors.New("boom"))
	ie.capture("c", nil)

	if ie.attempts != 3 {
		t.Errorf("expected attempts=3, got %d", ie.attempts)
	}
	if len(ie.errs) != 1 {
		t.Fatalf("expected 1 captured error, got %d", len(ie.errs))
	}
}

// TestEvaluateInstrumentInitErrs_PartialFailure_DegradedNotFatal_S02 验证部分注册
// 失败时：degraded=true 但 fatal=nil（不阻断，仅日志+标记，符合"不得 panic"设计决策）。
func TestEvaluateInstrumentInitErrs_PartialFailure_DegradedNotFatal_S02(t *testing.T) {
	ie := &instrumentInitErrs{attempts: 10}
	ie.errs = []error{errors.New("one instrument failed")}

	degraded, fatal := evaluateInstrumentInitErrs(ie)
	if !degraded {
		t.Error("expected degraded=true when any instrument fails")
	}
	if fatal != nil {
		t.Errorf("expected fatal=nil for partial failure, got %v", fatal)
	}
}

// TestEvaluateInstrumentInitErrs_AllFailed_Fatal_S02 验证全部 instrument 都失败
// （说明 meter provider 根本没起来）时才返回 fatal error，交由调用方降级处理。
func TestEvaluateInstrumentInitErrs_AllFailed_Fatal_S02(t *testing.T) {
	ie := &instrumentInitErrs{attempts: 3}
	ie.errs = []error{errors.New("e1"), errors.New("e2"), errors.New("e3")}

	degraded, fatal := evaluateInstrumentInitErrs(ie)
	if !degraded {
		t.Error("expected degraded=true")
	}
	if fatal == nil {
		t.Error("expected fatal error when all attempts failed")
	}
}

// TestEvaluateInstrumentInitErrs_NoFailure_S02 验证零失败时 degraded=false 且无 fatal。
func TestEvaluateInstrumentInitErrs_NoFailure_S02(t *testing.T) {
	ie := &instrumentInitErrs{attempts: 5}
	degraded, fatal := evaluateInstrumentInitErrs(ie)
	if degraded || fatal != nil {
		t.Errorf("expected no degradation, got degraded=%v fatal=%v", degraded, fatal)
	}
}

// TestInitInstruments_SetsPackageLevelVars_NotShadowed_S02 回归锚点：早期脚本化
// 重构曾错误地对包级变量使用 `InstrX, err := meter.Y(...)`——Go 的 := 只要求
// LHS 中至少一个标识符是"当前作用域内新的"，而包级变量与函数作用域是不同的
// scope，所以 InstrX 会被当作函数内新变量声明，遮蔽同名包级变量，导致包级
// Instr* 永远保持 nil（所有 Record* 调用点的 nil 判断会让指标永久静默丢失，
// 且不会被编译器发现，因为遮蔽变量本身用到了就不会报 unused）。
// 正确写法必须是 `InstrX, err = meter.Y(...)`（= 而非 :=，err 提前 var 声明）。
func TestInitInstruments_SetsPackageLevelVars_NotShadowed_S02(t *testing.T) {
	meter := noop.NewMeterProvider().Meter("test")
	ie := &instrumentInitErrs{}
	initInstruments(meter, ie)

	t.Cleanup(func() {
		InstrLLMCallsTotal = nil
		InstrOutboxProcessFailuresTotal = nil
		InstrToolOutcomeDecodeFailuresTotal = nil
	})

	if InstrLLMCallsTotal == nil {
		t.Error("InstrLLMCallsTotal 仍为 nil：包级变量可能被局部变量遮蔽")
	}
	if InstrOutboxProcessFailuresTotal == nil {
		t.Error("InstrOutboxProcessFailuresTotal 仍为 nil：包级变量可能被局部变量遮蔽")
	}
	if InstrToolOutcomeDecodeFailuresTotal == nil {
		t.Error("InstrToolOutcomeDecodeFailuresTotal 仍为 nil：包级变量可能被局部变量遮蔽")
	}
	if ie.attempts == 0 {
		t.Error("expected ie.attempts > 0 after initInstruments")
	}
	if len(ie.errs) != 0 {
		t.Errorf("noop meter 不应产生注册失败，got %d errs", len(ie.errs))
	}
}
