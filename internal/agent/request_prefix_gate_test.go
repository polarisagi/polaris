package agent

// ADR-0105 决策十一第二项：真实请求边界门控。驱动场景与判据见 request_prefix_gate_check_test.go，
// 夹具见 request_prefix_gate_fixture_test.go。未经负向验证的门控不算 landed——
// negative_* 子测试向真实请求路径注入已知破坏，断言门控会报红。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/compact"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// gateL0Len：L0 由两条 system 消息组成——阶段契约段（部署期常量）+ 可变稳定核。
const gateL0Len = 2

func gateCfgFor(e *gateEnv, tools string, skip map[int]bool) gateCfg {
	return gateCfg{l0Len: gateL0Len, skipTurns: skip, tools: tools, vault: e.vault, session: e.session}
}

func gateAssertClean(t *testing.T, e *gateEnv, tools string, skip map[int]bool) []gateReq {
	t.Helper()
	reqs := e.prov.snapshot()
	for _, msg := range checkRequestGate(reqs, gateCfgFor(e, tools, skip)) {
		t.Error(msg)
	}
	gateAssertNotVacuous(t, reqs)
	return reqs
}

// gateAssertNotVacuous：夹具确实把 PII、核心记忆与四类召回送进了请求，门控不是对空内容成立。
func gateAssertNotVacuous(t *testing.T, reqs []gateReq) {
	t.Helper()
	var all strings.Builder
	for _, r := range reqs {
		if !strings.Contains(r.msgs[gateL0Len-1].Content, "⟦PII:") {
			t.Errorf("%s：L0 可变稳定核里应含令牌化后的用户画像 PII", gateTag(r, 0))
		}
		for _, m := range r.msgs {
			all.WriteString(m.Content + "\n")
		}
	}
	for _, want := range append([]string{"billing contact ⟦PII:"}, gateRecallMarkers...) {
		if !strings.Contains(all.String(), want) {
			t.Errorf("夹具未生效：全部请求中没有 %q", want)
		}
	}
}

func gatePhaseCount(reqs []gateReq, turn int, phase string) int {
	n := 0
	for _, r := range reqs {
		if r.turn == turn && r.phase == phase {
			n++
		}
	}
	return n
}

func appendTurn(h []types.Message, input, reply string) []types.Message {
	return append(h, types.Message{Role: "user", Content: input}, types.Message{Role: "assistant", Content: reply})
}

// 回合 1：需要工具且经历一次重规划（Perceive→Plan→Execute→Reflect→Plan→Execute→Reflect→Respond）；
// 回合 2：同会话追加一句，期间会话内激活一个工具；回合 3：Perceive 合并直答。
func TestRequestPrefixGate_ThreeTurns(t *testing.T) {
	e := newGateEnv(t)
	h := gateHistory(2)
	in1 := "帮我把数据库迁移到新版本，备份发给 " + gateEmailAlice + "，手机 " + gatePhoneAlice
	h = appendTurn(h, in1, e.runTurn(1, h, in1, gateToolScript(true)))
	e.cat.ActivateTool(e.session, "extra_b")
	in2 := "再确认一下备份文件的保存位置"
	h = appendTurn(h, in2, e.runTurn(2, h, in2, gateToolScript(false)))
	e.runTurn(3, h, "迁移状态现在怎么样了", gateDirectScript())

	reqs := gateAssertClean(t, e, "plan-only", nil)
	if gatePhaseCount(reqs, 1, "PLAN") != 2 || gatePhaseCount(reqs, 1, "REFLECT") != 2 || gatePhaseCount(reqs, 3, "PERCEIVE") != 1 {
		t.Errorf("场景未按预期覆盖重规划/直答：%d plan, %d reflect, %d direct", gatePhaseCount(reqs, 1, "PLAN"),
			gatePhaseCount(reqs, 1, "REFLECT"), gatePhaseCount(reqs, 3, "PERCEIVE"))
	}
}

// 会话历史越过跳窗阈值的回合：L2 合法变化（锚定摘要），L0..L1 仍须是前缀；跳窗后的回合恢复纯追加。
func TestRequestPrefixGate_HistorySkipWindow(t *testing.T) {
	e := newGateEnv(t)
	h := gateHistory(19) // 默认窗口 20 条：19 条不跳窗
	h = appendTurn(h, "第一轮提问", e.runTurn(1, h, "第一轮提问", gateToolScript(false)))
	h = appendTurn(h, "第二轮提问", e.runTurn(2, h, "第二轮提问", gateToolScript(false))) // 21 条：跳窗
	e.runTurn(3, h, "第三轮提问", gateToolScript(false))                             // 23 条：同一锚点，纯追加

	reqs := gateAssertClean(t, e, "plan-only", map[int]bool{2: true})
	found := false
	for _, r := range reqs {
		if r.turn == 2 && strings.Contains(r.msgs[gateL1End(r.msgs, gateL0Len)].Content, "较早的 10 条对话已省略") {
			found = true
		}
	}
	if !found {
		t.Error("turn2 应发生历史跳窗（L2 首位为锚定摘要）")
	}
}

// uniform_tools：四阶段 tools 字节一致，非 Plan 阶段 tool_choice=none。
func TestRequestPrefixGate_UniformTools(t *testing.T) {
	setGateThresholds(t, func(th *config.Thresholds) { th.M4Kernel.CacheUniformTools = true })
	e := newGateEnv(t)
	h := gateHistory(2)
	h = appendTurn(h, "迁移数据库", e.runTurn(1, h, "迁移数据库", gateToolScript(true)))
	e.cat.ActivateTool(e.session, "extra_a")
	e.runTurn(2, h, "再确认一下备份", gateToolScript(false))
	gateAssertClean(t, e, "uniform", nil)
}

// PRM 候选路径：Plan 阶段并发 N 个候选请求（safecall.Infer 非流式），与其余阶段共享 L0..L2。
func gatePRMEnv(t *testing.T) *gateEnv {
	e := newGateEnv(t)
	scorer := &gateProvider{script: map[string][]scriptedReply{"aux": {{content: `{"score":0.8,"reason":"ok"}`}}}}
	e.prm = NewDefaultPRM(PRMConfig{Enabled: true, ComplexityGate: 0.3, MaxCandidates: 2, MinThreshold: 0.4}, scorer)
	return e
}

func gatePRMScript() map[string][]scriptedReply {
	dag, _ := toolCallsToDAGJSON([]types.InferToolCall{{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)}})
	s := gateToolScript(false)
	s["perceive"] = []scriptedReply{{content: `{"Goal":"迁移数据库并备份","NeedsTools":true,"Complexity":0.9}`}}
	s["plan"] = []scriptedReply{{content: string(dag)}}
	return s
}

func TestRequestPrefixGate_PRMCandidates(t *testing.T) {
	e := gatePRMEnv(t)
	h := gateHistory(2)
	h = appendTurn(h, "迁移数据库", e.runTurn(1, h, "迁移数据库", gatePRMScript()))
	e.runTurn(2, h, "再确认一下备份", gatePRMScript())
	reqs := gateAssertClean(t, e, "none", nil)
	if gatePhaseCount(reqs, 1, "PLAN") != 2 {
		t.Errorf("PRM 应并发 2 个 Plan 候选，实际 %d", gatePhaseCount(reqs, 1, "PLAN"))
	}
}

func TestRequestPrefixGate_PRMCandidatesUniformTools(t *testing.T) {
	setGateThresholds(t, func(th *config.Thresholds) { th.M4Kernel.CacheUniformTools = true })
	e := gatePRMEnv(t)
	h := gateHistory(2)
	h = appendTurn(h, "迁移数据库", e.runTurn(1, h, "迁移数据库", gatePRMScript()))
	e.runTurn(2, h, "再确认一下备份", gatePRMScript())
	gateAssertClean(t, e, "uniform", nil)
}

// 上下文压力：单次请求超过热路径压缩硬触发线（>90% 窗口）。压缩只能作用于 L0..L2 之后的回合内容，
// 不得把 L2 历史与 L3 阶段选择器卷进 LLM 摘要（否则每请求改写前缀、且模型看不到当前阶段）。
func TestRequestPrefixGate_HotPathCompactionKeepsPrefix(t *testing.T) {
	e := newGateEnv(t)
	e.cwm = NewContextWindowManager(9000)
	bigIntent := "迁移数据库并备份。" + strings.Repeat("0123456789", 1600)
	if _, err := e.tryTurn(1, gateBigHistory(20, 1200), bigIntent, withAuxSummary(gateToolScript(false))); err != nil {
		t.Errorf("压缩后回合应正常完成：%v", err)
	}
	reqs := e.prov.snapshot()
	for _, msg := range checkRequestGate(reqs, gateCfgFor(e, "plan-only", nil)) {
		t.Error(msg)
	}
	// 非空转判据：仅历史 + 意图这两段输入就已超过 9000 token 窗口的硬触发线（请求里还有 ~2K token 的契约段）。
	in := append(gateBigHistory(20, 1200), types.Message{Role: "user", Content: bigIntent})
	if est := compact.RoughTokens(in); est*10 <= 9000*9 {
		t.Errorf("场景未触发硬压缩线：输入估算 %d token，窗口 9000", est)
	}
}

// 溢出恢复：Provider 以 ErrContextOverflow 拒绝一次后确定性修剪重试。修剪应先动 L0..L2 之后的
// 回合内容（本例意图很长，足以缩回窗口），不得把 L2 历史截断——否则该次请求的前缀与同回合其它阶段失配。
func TestRequestPrefixGate_OverflowRecoveryKeepsPrefix(t *testing.T) {
	e := newGateEnv(t)
	e.prov.overflowOnce = map[string]bool{"PERCEIVE": true}       // 只有 Perceive 的 L4 含完整本轮意图
	bigIntent := "迁移数据库并备份。" + strings.Repeat("0123456789", 4000) // 回合内容 ≈40KB，远大于 12KB 的 L2
	e.runTurn(1, gateBigHistory(10, 1200), bigIntent, gateToolScript(false))
	reqs := e.prov.snapshot()
	for _, msg := range checkRequestGate(reqs, gateCfgFor(e, "plan-only", nil)) {
		t.Error(msg)
	}
	if gatePhaseCount(reqs, 1, "PERCEIVE") != 2 {
		t.Errorf("应出现 1 次溢出 + 1 次修剪重试共 2 个 Perceive 请求，实际 %d", gatePhaseCount(reqs, 1, "PERCEIVE"))
	}
}

func withAuxSummary(s map[string][]scriptedReply) map[string][]scriptedReply {
	s["aux"] = []scriptedReply{{content: "SUMMARY: earlier turn content compacted"}}
	return s
}

// ---- 负向验证：向真实请求路径注入已知破坏，门控必须报红 ----

func gateInsertAt(msgs []types.Message, i int, m types.Message) []types.Message {
	out := append([]types.Message(nil), msgs[:i]...)
	out = append(out, m)
	return append(out, msgs[i:]...)
}

// runNegative 以 perturb 包装 Provider 跑一个工具回合，返回门控违规列表。
func runNegative(t *testing.T, perturb perturbFn, replan bool) []string {
	t.Helper()
	e := newGateEnv(t)
	e.wrap = func(p protocol.Provider) protocol.Provider {
		return &perturbProvider{Provider: p, fn: perturb, counts: map[string]int{}}
	}
	e.runTurn(1, gateHistory(4), "迁移数据库，联系 "+gateEmailAlice, gateToolScript(replan))
	return checkRequestGate(e.prov.snapshot(), gateCfgFor(e, "plan-only", nil))
}

func expectViolation(t *testing.T, vio []string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		ok := false
		for _, v := range vio {
			if strings.Contains(v, s) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("门控应报出含 %q 的违规，实际：%v", s, vio)
		}
	}
}

func TestRequestPrefixGate_Negative(t *testing.T) {
	t.Run("negative_random_message_inserted_after_L1", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "PLAN" {
				msgs = gateInsertAt(msgs, gateL1End(msgs, gateL0Len), types.Message{Role: "user", Content: "noise-" + randHex()})
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[a 同回合前缀]", "层 L2")
	})
	t.Run("negative_relevant_context_side_channel", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "REFLECT" {
				msgs = gateInsertAt(msgs, gateL1End(msgs, gateL0Len), types.Message{Role: "system", Content: "Relevant Context:\n- x"})
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[e 旁路]", "[a 同回合前缀]")
	})
	t.Run("negative_random_pii_token_per_request", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			for i := range msgs {
				msgs[i].Content = gateTokenRe.ReplaceAllStringFunc(msgs[i].Content, func(string) string { return "⟦PII:" + randHex() + "⟧" })
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[f PII]", "[a 同回合前缀]", "[c L0]")
	})
	t.Run("negative_compaction_rewrites_history", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "RESPOND" {
				i := gateL1End(msgs, gateL0Len)
				msgs[i].Content = "[compacted] " + msgs[i].Content
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[a 同回合前缀]", "层 L2")
	})
	t.Run("negative_l0_varies_per_phase", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "RESPOND" {
				msgs[gateL0Len-1].Content += "\n当前时间 " + randHex()
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[c L0]", "层 L0[可变稳定核]")
	})
	t.Run("negative_contract_segment_mutated", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "PLAN" {
				msgs[0].Content += "\n# session " + randHex()
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[c L0]", "层 L0[阶段契约]")
	})
	t.Run("negative_tools_reordered", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			if phase == "PLAN" && n == 1 {
				var cur types.InferOptions
				for _, f := range o {
					f(&cur)
				}
				rev := make([]types.ToolSchema, len(cur.Tools))
				for i, ts := range cur.Tools {
					rev[len(rev)-1-i] = ts
				}
				o = append(append([]types.InferOption(nil), o...), types.WithTools(rev))
			}
			return msgs, o
		}, true)
		expectViolation(t, vio, "[d tools]")
	})
	t.Run("negative_cache_breakpoint_lost", func(t *testing.T) {
		vio := runNegative(t, func(phase string, n int, msgs []types.Message, o []types.InferOption) ([]types.Message, []types.InferOption) {
			for i := range msgs {
				msgs[i].CacheBreakpoint = false
			}
			return msgs, o
		}, false)
		expectViolation(t, vio, "[层信息]")
	})
}
