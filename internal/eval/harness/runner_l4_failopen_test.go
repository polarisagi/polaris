package harness

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

// 本文件守护 2026-08-11 修复的 fail-open：Level4LLMJudge 用例在**没有 Judge
// Provider** 的环境里，此前被静默按通过计（源码注释原文："若无注入 Provider
// 则静默跳过（退化为已通过的字符串检查结果）"）。
//
// 而同一文件里 "Provider 存在但调用失败" 分支对 P0 用例是 fail-safe 的。
// 两者本是同一件事——**无法完成语义评判**——却给了相反的处置，且更常见的那个
// （CI 无 Provider）恰恰落在不安全的一侧。
//
// 这条防护重要在：eval 门控是 HE-4 的执行面。它自己 fail-open，等于整条
// 防退化链路失效，而 CI 依然是绿的（ADR-0091：门控失效与门控通过长得一样）。

func newFailOpenTestRunner(t *testing.T) *RunnerImpl {
	t.Helper()
	// store / evalStore 传 nil：evaluate 不触碰它们（r.agent 为 nil 时直接把
	// input 当 output），本用例只验 L4 分支的判定，不涉及持久化。
	return NewRunner(nil, nil, config.DefaultThresholds(), config.EvalConfig{})
}

func TestL4WithoutProvider_P0MustNotPass(t *testing.T) {
	r := newFailOpenTestRunner(t)
	// 刻意不注入 llmProvider —— 复现 CI 的真实环境。
	c := &EvalCase{
		ID:       "p0-no-provider",
		Level:    Level4LLMJudge,
		Severity: SeverityP0,
		Input:    map[string]any{"q": "任意输入"},
		Expected: map[string]any{"criteria": "必须经语义评判"},
	}

	passed, safetyFail, unjudged := r.evaluate(context.Background(), c)

	if passed {
		t.Error("P0 的 L4 用例在无 Judge Provider 时必须判失败（fail-safe）——" +
			"评不了就不能算过，否则 HE-4 门控在 CI 里恒绿而实际什么都没校验")
	}
	if !unjudged {
		t.Error("必须标记为 unjudged，否则报告无法区分「真评过并通过」与「没评过」")
	}
	_ = safetyFail
}

func TestL4WithoutProvider_NonP0MarkedUnjudged(t *testing.T) {
	r := newFailOpenTestRunner(t)
	c := &EvalCase{
		ID:       "p1-no-provider",
		Level:    Level4LLMJudge,
		Severity: SeverityP1,
		Input:    map[string]any{"q": "任意输入"},
		Expected: map[string]any{"criteria": "必须经语义评判"},
	}

	passed, _, unjudged := r.evaluate(context.Background(), c)

	// 非 P0 沿用字符串检查结果（不把整条流水线卡死在还没配 Judge 的阶段），
	// 但**必须**计入 unjudged——这是"这条结论没经过语义评判"的唯一凭据。
	if !passed {
		t.Error("非 P0 用例应沿用字符串检查结果，不因缺 Provider 而判失败")
	}
	if !unjudged {
		t.Error("非 P0 用例缺 Provider 时必须标记 unjudged，否则一份 pass_count 很高的" +
			"报告可能一条都没真正评过，而报告本身看不出来")
	}
}

func TestNonL4Unaffected(t *testing.T) {
	r := newFailOpenTestRunner(t)
	// L1 用例全程离线，不该被本次改动波及，也不该被计入 unjudged。
	c := &EvalCase{
		ID:       "l1-offline",
		Level:    Level1Assert,
		Severity: SeverityP0,
		Input:    map[string]any{"q": "hello"},
		Expected: map[string]any{"output": "hello"},
	}

	passed, _, unjudged := r.evaluate(context.Background(), c)

	if !passed {
		t.Error("L1 断言用例（输出含期望子串）应通过")
	}
	if unjudged {
		t.Error("非 L4 用例不得计入 unjudged —— 它们本就不需要语义评判")
	}
}

// TestRunSuiteCountsUnjudged 验证计数确实汇总进了报告。
// evaluate 判对但 RunSuite 忘了累加，等于修了一半——报告依然看不出问题。
func TestRunSuiteCountsUnjudged(t *testing.T) {
	r := newFailOpenTestRunner(t)
	cases := []*EvalCase{
		{ID: "a", Level: Level4LLMJudge, Severity: SeverityP1, Input: map[string]any{}, Expected: map[string]any{}},
		{ID: "b", Level: Level1Assert, Severity: SeverityP1, Input: map[string]any{}, Expected: map[string]any{}},
	}
	unjudgedCount := 0
	for _, c := range cases {
		if _, _, unjudged := r.evaluate(context.Background(), c); unjudged {
			unjudgedCount++
		}
	}
	if unjudgedCount != 1 {
		t.Errorf("应有且仅有 1 条 L4 用例被标记 unjudged，实际 %d", unjudgedCount)
	}
}
