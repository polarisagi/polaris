package config

import "testing"

// 阶段思考档位写错须在加载时失败，不得静默回退成 Provider 默认（默认即 high，会悄悄多花推理 token）。
func TestM4KernelValidate_ThinkingModes(t *testing.T) {
	ok := DefaultThresholds().M4Kernel
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if ok.ThinkingPerceive != "low" || ok.ThinkingReflect != "low" || ok.ThinkingPlanInitial != "high" || ok.ThinkingRespond != "" {
		t.Fatalf("unexpected thinking defaults: %+v", ok)
	}
	bad := ok
	bad.ThinkingReflect = "medium"
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid thinking mode must be rejected")
	}
}
