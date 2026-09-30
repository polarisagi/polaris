package agent

// 请求边界门控的判据（对**实际请求**）。层边界判据：
//   - L3 起点 = 第一条含阶段选择器（行首 "# ACTIVE PHASE: X"）的 system 消息，记为 sel；
//     选择器恒为 L3 首条（Perceive/Plan/Reflect/Respond 四处都先写契约位），故 L0..L2 = msgs[:sel]。
//   - 层信息旁证：msgs[sel-1]（L2 末，L2 空则 L1 末）必须带 CacheBreakpoint，且 msgs[0]、msgs[l0Len-1] 也带；
//     否则说明层信息在请求路径上丢失，或 L2 末与选择器之间被插入了消息。
//   - L0 = 前 l0Len 条（契约段 + 可变稳定核）；L1 = 其后连续的 system 消息；L2 = 其后直到 sel 的历史消息。
// 失败信息给出首个分歧的消息下标、所属层与分歧字节处的上下文。

import (
	"fmt"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/types"
)

type gateCfg struct {
	l0Len     int
	skipTurns map[int]bool // 该回合相对上一回合发生了历史跳窗：只要求 L0..L1 是前缀
	tools     string       // "plan-only"：仅 Plan 下发；"uniform"：各阶段都下发；"none"：都不下发
	vault     *guard.PIITokenVault
	session   string
}

func gateSel(msgs []types.Message) int {
	for i, m := range msgs {
		if m.Role == "system" && gatePhaseRe.MatchString(m.Content) {
			return i
		}
	}
	return -1
}

func gateL1End(msgs []types.Message, l0Len int) int {
	i := l0Len
	for i < len(msgs) && msgs[i].Role == "system" {
		i++
	}
	return i
}

func gateLayer(msgs []types.Message, idx, l0Len int) string {
	switch {
	case idx < 0 || idx >= len(msgs):
		return "越界"
	case idx == 0 && l0Len > 1:
		return "L0[阶段契约]"
	case idx < l0Len:
		return "L0[可变稳定核]"
	case idx < gateL1End(msgs, l0Len):
		return "L1"
	}
	return "L2"
}

func sameGateMsg(a, b types.Message, withFlag bool) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ReasoningContent != b.ReasoningContent || len(a.Parts) != len(b.Parts) {
		return false
	}
	return !withFlag || a.CacheBreakpoint == b.CacheBreakpoint
}

// gateFirstDiff 返回 a、b 首个分歧的下标与说明；相同返回 -1。只比较 n 条。
func gateFirstDiff(a, b []types.Message, n int, withFlag bool) (int, string) {
	for i := 0; i < n; i++ {
		if i >= len(a) || i >= len(b) {
			return i, fmt.Sprintf("长度不足（a=%d b=%d）", len(a), len(b))
		}
		if sameGateMsg(a[i], b[i], withFlag) {
			continue
		}
		if a[i].Role != b[i].Role {
			return i, fmt.Sprintf("role %q vs %q", a[i].Role, b[i].Role)
		}
		if a[i].Content != b[i].Content {
			k := 0
			for k < len(a[i].Content) && k < len(b[i].Content) && a[i].Content[k] == b[i].Content[k] {
				k++
			}
			return i, fmt.Sprintf("首个分歧字节 offset=%d：%q vs %q", k, gateSnip(a[i].Content, k), gateSnip(b[i].Content, k))
		}
		return i, "CacheBreakpoint/ReasoningContent/Parts 不一致"
	}
	return -1, ""
}

func gateSnip(s string, at int) string {
	lo, hi := max(at-12, 0), min(at+24, len(s))
	return s[lo:hi]
}

func gateTag(r gateReq, n int) string { return fmt.Sprintf("turn%d/%s#%d", r.turn, r.phase, n) }

// checkRequestGate 对记录下来的全部请求执行 a–f 六条断言，返回违规列表（空=通过）。
func checkRequestGate(reqs []gateReq, cfg gateCfg) []string {
	var v []string
	if len(reqs) == 0 {
		return []string{"没有记录到任何请求"}
	}
	byTurn := map[int][]int{}
	sel := make([]int, len(reqs))
	for i, r := range reqs {
		byTurn[r.turn] = append(byTurn[r.turn], i)
		sel[i] = gateSel(r.msgs)
	}
	turns := make([]int, 0, len(byTurn))
	for t := range byTurn {
		turns = append(turns, t)
	}
	sort.Ints(turns)

	v = append(v, gateCheckLayerInfo(reqs, sel, cfg)...)
	if len(v) > 0 {
		return v // 层边界都找不到，后续断言无意义
	}
	v = append(v, gateCheckSameTurn(reqs, sel, byTurn, turns, cfg)...)      // a
	v = append(v, gateCheckAdjacentTurns(reqs, sel, byTurn, turns, cfg)...) // b
	v = append(v, gateCheckL0(reqs, cfg)...)                                // c
	v = append(v, gateCheckTools(reqs, byTurn, turns, cfg)...)              // d
	v = append(v, gateCheckSideChannel(reqs, sel)...)                       // e
	v = append(v, gateCheckPIITokens(reqs, cfg)...)                         // f
	return v
}

func gateCheckLayerInfo(reqs []gateReq, sel []int, cfg gateCfg) []string {
	var v []string
	counts := map[string]int{}
	for i, r := range reqs {
		n := counts[r.phase]
		counts[r.phase]++
		tag := gateTag(r, n)
		if sel[i] < cfg.l0Len {
			v = append(v, fmt.Sprintf("[层信息] %s：找不到阶段选择器（sel=%d）", tag, sel[i]))
			continue
		}
		for _, bp := range []int{0, cfg.l0Len - 1, sel[i] - 1} {
			if !r.msgs[bp].CacheBreakpoint {
				v = append(v, fmt.Sprintf("[层信息] %s：msgs[%d]（%s）缺少 CacheBreakpoint——层信息在请求路径上丢失，或 L2 末与阶段选择器之间被插入了消息",
					tag, bp, gateLayer(r.msgs, bp, cfg.l0Len)))
			}
		}
	}
	return v
}

// a：同一回合所有阶段请求的 L0..L2 字节一致（含 CacheBreakpoint 标记）。
func gateCheckSameTurn(reqs []gateReq, sel []int, byTurn map[int][]int, turns []int, cfg gateCfg) []string {
	var v []string
	for _, t := range turns {
		idxs := byTurn[t]
		base := idxs[0]
		for k, i := range idxs[1:] {
			n := min(sel[base], sel[i])
			if d, why := gateFirstDiff(reqs[base].msgs, reqs[i].msgs, n, true); d >= 0 {
				v = append(v, fmt.Sprintf("[a 同回合前缀] %s 与 %s 在 msgs[%d]（层 %s）分歧：%s",
					gateTag(reqs[i], k+1), gateTag(reqs[base], 0), d, gateLayer(reqs[base].msgs, d, cfg.l0Len), why))
			} else if sel[i] != sel[base] {
				v = append(v, fmt.Sprintf("[a 同回合前缀] %s 的 L0..L2 长度 %d 与 %s 的 %d 不一致：前 %d 条相同，多出/缺少的消息起自 msgs[%d]（层 %s）",
					gateTag(reqs[i], k+1), sel[i], gateTag(reqs[base], 0), sel[base], n, n, gateLayer(reqs[base].msgs, n, cfg.l0Len)))
			}
		}
	}
	return v
}

// b：相邻未跳窗回合，前一回合的 L0..L2 是后一回合的严格前缀（不比较 CacheBreakpoint：L2 末标记随追加后移）。
func gateCheckAdjacentTurns(reqs []gateReq, sel []int, byTurn map[int][]int, turns []int, cfg gateCfg) []string {
	var v []string
	for k := 1; k < len(turns); k++ {
		ia, ib := byTurn[turns[k-1]][0], byTurn[turns[k]][0]
		a, b := reqs[ia].msgs, reqs[ib].msgs
		n := sel[ia]
		if cfg.skipTurns[turns[k]] {
			n = gateL1End(a, cfg.l0Len) // 跳窗：L2 合法变化，L0..L1 仍须一致
		} else if sel[ib] <= sel[ia] {
			v = append(v, fmt.Sprintf("[b 相邻回合] turn%d 的 L0..L2 长度 %d 未大于 turn%d 的 %d（历史应只追加）", turns[k], sel[ib], turns[k-1], sel[ia]))
			continue
		}
		if d, why := gateFirstDiff(a, b, n, false); d >= 0 {
			v = append(v, fmt.Sprintf("[b 相邻回合] turn%d 的 L0..L2 不是 turn%d 的前缀：msgs[%d]（层 %s）分歧：%s",
				turns[k-1], turns[k], d, gateLayer(a, d, cfg.l0Len), why))
		}
	}
	return v
}

// c：L0 在全部回合全部阶段字节一致，且契约段恰为部署期常量。
func gateCheckL0(reqs []gateReq, cfg gateCfg) []string {
	var v []string
	want := configs.PhaseContractsSection()
	for i, r := range reqs {
		if r.msgs[0].Content != want {
			v = append(v, fmt.Sprintf("[c L0] %s：msgs[0] 不是 configs.PhaseContractsSection() 常量", gateTag(r, i)))
		}
		if d, why := gateFirstDiff(reqs[0].msgs, r.msgs, cfg.l0Len, true); d >= 0 {
			v = append(v, fmt.Sprintf("[c L0] %s 与首个请求在 msgs[%d]（层 %s）分歧：%s", gateTag(r, i), d, gateLayer(r.msgs, d, cfg.l0Len), why))
		}
	}
	return v
}

// d：tools 数组——同回合字节一致；跨回合前一回合是后一回合的元素级前缀（激活只追加）。
func gateCheckTools(reqs []gateReq, byTurn map[int][]int, turns []int, cfg gateCfg) []string {
	var v []string
	var prevNames []string
	var prevTurn int
	for _, t := range turns {
		var first *gateReq
		for k, i := range byTurn[t] {
			r := reqs[i]
			has := r.toolsJSON != ""
			want := map[string]bool{"uniform": true, "none": false, "plan-only": r.phase == "PLAN"}[cfg.tools]
			if has != want {
				v = append(v, fmt.Sprintf("[d tools] %s：tools 下发=%v，按 %q 模式应为 %v", gateTag(r, k), has, cfg.tools, want))
			}
			if cfg.tools == "uniform" && r.phase != "PLAN" && r.toolChoice != "none" {
				v = append(v, fmt.Sprintf("[d tools] %s：uniform 模式非 Plan 阶段须 tool_choice=none，实际 %q", gateTag(r, k), r.toolChoice))
			}
			if !has {
				continue
			}
			if first == nil {
				rr := r
				first = &rr
				continue
			}
			if r.toolsJSON != first.toolsJSON {
				v = append(v, fmt.Sprintf("[d tools] %s 的 tools 与同回合 %s 字节不一致：%v vs %v", gateTag(r, k), gateTag(*first, 0), r.toolNames, first.toolNames))
			}
		}
		if first == nil {
			continue
		}
		if prevNames != nil {
			if len(first.toolNames) < len(prevNames) || strings.Join(first.toolNames[:len(prevNames)], ",") != strings.Join(prevNames, ",") {
				v = append(v, fmt.Sprintf("[d tools] turn%d 的 tools %v 不是 turn%d 的 %v 的追加延伸（激活只能追加在尾部）", t, first.toolNames, prevTurn, prevNames))
			}
		}
		prevNames, prevTurn = first.toolNames, t
	}
	return v
}

// e：无旁路注入；召回只出现在阶段选择器（L3 起点）之后。
func gateCheckSideChannel(reqs []gateReq, sel []int) []string {
	var v []string
	for i, r := range reqs {
		for j, m := range r.msgs {
			if strings.Contains(m.Content, "Relevant Context:") {
				v = append(v, fmt.Sprintf("[e 旁路] %s：msgs[%d] 含旁路注入 \"Relevant Context:\"", gateTag(r, i), j))
			}
			for _, mk := range gateRecallMarkers {
				if strings.Contains(m.Content, mk) && j <= sel[i] {
					v = append(v, fmt.Sprintf("[e 召回位置] %s：%s 出现在 msgs[%d]，不在 L3 选择器(%d)之后——召回打断了共享前缀", gateTag(r, i), mk, j, sel[i]))
				}
			}
		}
	}
	return v
}

// f：PII 原文不发往 Provider；同一原文在全部请求（含跨回合）中是同一令牌。
// 判据：全部请求出现的不同令牌数 == 夹具中不同 PII 原文数，且每个令牌都能在 vault 中还原、还原值互不相同。
func gateCheckPIITokens(reqs []gateReq, cfg gateCfg) []string {
	var v []string
	tokens := map[string]bool{}
	for i, r := range reqs {
		for j, m := range r.msgs {
			for _, o := range gateOriginals {
				if strings.Contains(m.Content, o) {
					v = append(v, fmt.Sprintf("[f PII] %s：msgs[%d] 含未令牌化的 PII 原文 %q", gateTag(r, i), j, o))
				}
			}
			for _, tok := range gateTokenRe.FindAllString(m.Content, -1) {
				tokens[tok] = true
			}
		}
	}
	if len(tokens) != len(gateOriginals) {
		v = append(v, fmt.Sprintf("[f PII] 全部请求共出现 %d 个不同令牌，应恰为 %d（同一原文须是同一令牌）：%v", len(tokens), len(gateOriginals), sortedKeys(tokens)))
	}
	seen := map[string]string{}
	for tok := range tokens {
		orig, err := cfg.vault.ResolveForTask(cfg.session, tok)
		if err != nil {
			v = append(v, fmt.Sprintf("[f PII] 令牌 %s 在 vault 中无法还原（会话内映射被提前清除？）：%v", tok, err))
			continue
		}
		if other, dup := seen[orig]; dup {
			v = append(v, fmt.Sprintf("[f PII] 同一原文 %q 对应多个令牌 %s / %s", orig, other, tok))
		}
		seen[orig] = tok
	}
	return v
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
