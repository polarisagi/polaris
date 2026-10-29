package fsm

import (
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

// TestLoopEffect_AudienceByTurnOrigin 交互回合面向用户流式输出；
// explicit（委派子智能体）与后台（cron / workflow）回合保持内部受众（R5-02）。
func TestLoopEffect_AudienceByTurnOrigin(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	cases := []struct {
		name       string
		completion string
		background bool
		want       protocol.LLMAudience
	}{
		{"auto 交互", "auto", false, protocol.AudienceUser},
		{"空值按 auto", "", false, protocol.AudienceUser},
		{"explicit 委派", "explicit", false, protocol.AudienceInternal},
		{"后台回合", "auto", true, protocol.AudienceInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sCtx := &StateContext{}
			sCtx.LoopCompletion = tc.completion
			sCtx.BackgroundTurn = tc.background
			eff, ok := sm.loopEffect(sCtx).(protocol.LLMFillEffect)
			if !ok {
				t.Fatal("loopEffect 应返回 LLMFillEffect")
			}
			if eff.Audience != tc.want {
				t.Errorf("Audience = %d, want %d", eff.Audience, tc.want)
			}
		})
	}
}

// TestResetAgenticTurn_ClearsLoopStream 流式标记是回合级的：新回合开始必须清零，
// 否则上一回合的「已流式」会让本回合终态漏发回复；BackgroundTurn 则不得被清掉。
func TestResetAgenticTurn_ClearsLoopStream(t *testing.T) {
	sCtx := &StateContext{}
	sCtx.BackgroundTurn = true
	sCtx.Mu.Lock()
	sCtx.RecordLoopStreamedLocked("上一回合")
	sCtx.Mu.Unlock()

	sCtx.ResetAgenticTurn()

	if sCtx.LoopReplyStreamed || sCtx.LoopStreamedText != "" {
		t.Errorf("ResetAgenticTurn 后流式状态应清零: %+v", sCtx.LoopStream)
	}
	if !sCtx.BackgroundTurn {
		t.Error("BackgroundTurn 属回合来源，不应被 ResetAgenticTurn 清掉")
	}
}

// TestLoopEffect_ModelPoolFromLoopKey 主循环的模型池取自独立键 model_pool.loop，
// 不再复用规划池 plan_initial（二者语义不同：规划池参与升级阶梯，主循环不升级）（R5-07）。
func TestLoopEffect_ModelPoolFromLoopKey(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	prev := config.Get()
	t.Cleanup(func() { config.Update(prev) })

	cfg := config.Config{}
	cfg.Thresholds = config.DefaultThresholds()
	cfg.Thresholds.M4Kernel.ModelPoolPlanInitial = "reasoning" // 调贵规划池不应牵连主循环
	cfg.Thresholds.M4Kernel.ModelPoolLoop = "general"
	config.Update(&cfg)

	eff := sm.loopEffect(&StateContext{}).(protocol.LLMFillEffect)
	if eff.ModelPool != "general" {
		t.Errorf("ModelPool = %q，期望取自 model_pool.loop=general", eff.ModelPool)
	}

	cfg.Thresholds.M4Kernel.ModelPoolLoop = ""
	config.Update(&cfg)
	eff = sm.loopEffect(&StateContext{}).(protocol.LLMFillEffect)
	if eff.ModelPool != "default" {
		t.Errorf("model_pool.loop 为空应回落 default，实际 %q", eff.ModelPool)
	}
}

func TestDefaultThresholds_ModelPoolLoopDefault(t *testing.T) {
	if got := config.DefaultThresholds().M4Kernel.ModelPoolLoop; got != "default" {
		t.Errorf("model_pool.loop 默认值 = %q，期望 default", got)
	}
	bad := config.DefaultThresholds().M4Kernel
	bad.ModelPoolLoop = "bogus"
	if err := bad.Validate(); err == nil {
		t.Error("非法池名应被校验拒绝")
	}
}
