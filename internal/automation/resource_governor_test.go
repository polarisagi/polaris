package automation

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

func TestResourceGovernor_AdmitPriority(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	// Override probes for deterministic test
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=1 admit under normal pressure
	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("priority=1 should admit under normal load")
	}
	rg.Release()

	// priority=5 admit under normal pressure
	if !(func() bool { ok, _ := rg.Admit(5); return ok })() {
		t.Errorf("priority=5 should admit under normal load")
	}
	rg.Release()
}

func TestResourceGovernor_MemoryPressure(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 256 } // below 512MB
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit even under memory pressure
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under memory pressure
	if (func() bool { ok, _ := rg.Admit(3); return ok })() {
		t.Errorf("priority=3 should be rejected under memory pressure (<512MB free)")
	}
}

func TestResourceGovernor_CPUThreshold(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 80.0 }

	// priority=0 always admit
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under high CPU
	if (func() bool { ok, _ := rg.Admit(3); return ok })() {
		t.Errorf("priority=3 should be rejected under high CPU (>70%%)")
	}
}

func TestResourceGovernor_ConcurrentLimit(t *testing.T) {
	rg := NewResourceGovernor(3, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill all 3 slots
	for i := 0; i < 3; i++ {
		if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
			t.Fatalf("slot %d: should admit", i)
		}
	}

	// 4th task rejected at capacity
	if (func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("4th task should be rejected (capacity=3)")
	}

	// priority=0 always admitted even at capacity
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit even at capacity")
	}
	rg.Release()

	// Release and retry
	rg.Release()
	rg.Release()
	rg.Release()

	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("after release, should admit")
	}
	rg.Release()
}

func TestResourceGovernor_WaitForCapacity(t *testing.T) {
	rg := NewResourceGovernor(1, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill the only slot
	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Fatalf("should admit first task")
	}

	// Release in background
	go func() {
		rg.Release()
	}()

	ctx := context.Background()
	if err := rg.WaitForCapacity(ctx); err != nil {
		t.Errorf("WaitForCapacity should succeed, got %v", err)
	}
}

// TestAdmitLLM_UserFacingNeverDeniedByPressure 锁定 2026-09-22 的策略裁决：
// 内存/CPU 水位线不得拒绝用户可见推理（priority=0）。
//
// 回归背景：旧实现把水位线套在所有 priority != 0 的调用上，而 AdmitLLM 全仓
// 唯一调用方恒传 1——唯一被拦的恰恰是交互式对话，用户侧表现为等待 60 秒后
// 收到一句无法归因的"推理返回空内容，请检查模型配置或重试"。
func TestAdmitLLM_UserFacingNeverDeniedByPressure(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{}).WithMaxConcurrentLLM(4)
	// 内存濒死 + CPU 打满：最严苛的水位线条件
	rg.memProbeFn = func() int64 { return 1 }
	rg.cpuProbeFn = func() float64 { return 100.0 }

	admitted, level := rg.AdmitLLM(0)
	if !admitted {
		t.Fatalf("用户可见推理不得因资源水位线被拒（degrade level %d）", level)
	}
	rg.ReleaseLLM()
	if level == 0 {
		t.Error("degradeLevel 仍应如实回报压力等级，供调用方做质量降级")
	}

	// 后台可降级推理在同等条件下必须被拒。
	if admitted, _ := rg.AdmitLLM(1); admitted {
		t.Error("后台可降级推理应在内存濒死时被拒")
	}
}

// TestAdmitLLM_ConcurrencyStillEnforced 并发上限对用户可见推理依然有效——
// 它是吞吐控制且有配套的 WaitForLLMCapacity 等待机制，与水位线闸门性质不同。
func TestAdmitLLM_ConcurrencyStillEnforced(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{}).WithMaxConcurrentLLM(2)
	rg.memProbeFn = func() int64 { return 8192 }
	rg.cpuProbeFn = func() float64 { return 10.0 }

	for i := range 2 {
		if admitted, _ := rg.AdmitLLM(0); !admitted {
			t.Fatalf("第 %d 次准入不应被拒", i+1)
		}
	}
	if admitted, _ := rg.AdmitLLM(0); admitted {
		t.Error("超过并发上限后应被拒")
	}
	rg.ReleaseLLM()
	if admitted, _ := rg.AdmitLLM(0); !admitted {
		t.Error("释放额度后应可再次准入")
	}
}

// TestAdmitBackground 覆盖后台准入闸门的三条关键语义。
func TestAdmitBackground(t *testing.T) {
	t.Run("压力下拒绝", func(t *testing.T) {
		rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
		rg.memProbeFn = func() int64 { return 256 }
		rg.cpuProbeFn = func() float64 { return 10.0 }
		if _, ok := rg.AdmitBackground("test"); ok {
			t.Error("内存低于 L2 时后台工作应被拒")
		}
	})

	t.Run("不触发活跃回调", func(t *testing.T) {
		rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
		rg.memProbeFn = func() int64 { return 8192 }
		rg.cpuProbeFn = func() float64 { return 10.0 }
		called := false
		rg.OnActivity(func() { called = true })

		release, ok := rg.AdmitBackground("test")
		if !ok {
			t.Fatal("资源充足时应准入")
		}
		release()
		if called {
			t.Error("后台工作不得打活跃标记——那会把 IdleEvolutionScheduler 的空闲窗口永久顶掉")
		}
		// 对照：用户路径必须打标记
		if _, _ = rg.Admit(0); !called {
			t.Error("用户请求应触发活跃回调")
		}
	})

	t.Run("release 幂等", func(t *testing.T) {
		rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
		rg.memProbeFn = func() int64 { return 8192 }
		rg.cpuProbeFn = func() float64 { return 10.0 }
		release, ok := rg.AdmitBackground("test")
		if !ok {
			t.Fatal("应准入")
		}
		release()
		release() // 重复调用不得把 inFlight 减成负数
		if got := rg.InFlight(); got != 0 {
			t.Errorf("InFlight = %d, want 0", got)
		}
	})

	t.Run("nil 接收者放行", func(t *testing.T) {
		var rg *ResourceGovernor
		release, ok := rg.AdmitBackground("test")
		if !ok || release == nil {
			t.Error("治理器缺席时应放行，且 release 可安全调用")
		}
		release()
	})
}

// TestAdmitLLM_OnlyUserFacingMarksActivity 后台推理不得刷新"用户活跃"时间：
// 否则空闲自进化自己的推理会把空闲窗口顶掉，自进化再也等不到下一个窗口。
func TestAdmitLLM_OnlyUserFacingMarksActivity(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{}).WithMaxConcurrentLLM(4)
	rg.memProbeFn = func() int64 { return 8192 }
	rg.cpuProbeFn = func() float64 { return 10.0 }
	marks := 0
	rg.OnActivity(func() { marks++ })

	if admitted, _ := rg.AdmitLLM(1); !admitted {
		t.Fatal("无压力时后台推理应准入")
	}
	if marks != 0 {
		t.Fatal("后台推理不得打活跃标记")
	}
	if admitted, _ := rg.AdmitLLM(0); !admitted {
		t.Fatal("用户可见推理应准入")
	}
	if marks != 1 {
		t.Fatalf("用户可见推理应打活跃标记，got %d", marks)
	}
}
