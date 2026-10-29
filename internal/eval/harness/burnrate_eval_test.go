package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

// 燃烧率误报 Eval（HE-4）：KillSwitch Stage1 的触发线必须同时满足
//  1. 正常会话（含缓存未命中的突发、并发子智能体）永不越线 —— 不误杀长任务；
//  2. 失控样本（死循环级持续非缓存 token）在有限时间内越线 —— 仍能防浪费。
// 阈值（metrics.DefaultThrottleBaseline、ThrottleMultiplier、EMA_30s 判据）由本评测的候选扫描选定，
// 数据依据见 state.yaml token_burn_rate 的 2026-10-28 复核。

type burnEvent struct {
	at     float64 // 入账时刻（相对起点，秒）
	tokens int64
}

type burnSample struct {
	id      string
	events  []burnEvent
	horizon float64 // 重放时长（秒），之后再空转 60s 观察衰减
}

type realSession struct {
	Calls []struct {
		StartMs   int64 `json:"start_ms"`
		LatencyMs int64 `json:"latency_ms"`
		Input     int64 `json:"input"`
		CacheHit  int64 `json:"cache_hit"`
		Output    int64 `json:"output"`
	} `json:"calls"`
}

// burnTokens 与 internal/llm/adapter.burnTokens 同口径：非缓存输入 + 输出。
func burnTokens(input, cacheHit, output int64) int64 {
	return max(input-cacheHit, 0) + output
}

func loadRealSession(t *testing.T) burnSample {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "burnrate_real_session.json"))
	require.NoError(t, err)
	var rs realSession
	require.NoError(t, json.Unmarshal(data, &rs))
	require.Len(t, rs.Calls, 12)
	s := burnSample{id: "real_session_12_calls"}
	for _, c := range rs.Calls {
		s.events = append(s.events, burnEvent{at: float64(c.StartMs+c.LatencyMs) / 1000, tokens: burnTokens(c.Input, c.CacheHit, c.Output)})
	}
	s.horizon = 60
	return s
}

func steady(id string, from, to, every float64, tokens int64) burnSample {
	s := burnSample{id: id, horizon: to}
	for at := from; at < to; at += every {
		s.events = append(s.events, burnEvent{at: at, tokens: tokens})
	}
	return s
}

func withEvent(base burnSample, id string, at float64, tokens int64) burnSample {
	out := burnSample{id: id, horizon: base.horizon, events: append([]burnEvent{}, base.events...)}
	out.events = append(out.events, burnEvent{at: at, tokens: tokens})
	sort.Slice(out.events, func(i, j int) bool { return out.events[i].at < out.events[j].at })
	return out
}

// normalSamples 全部是应当放行的会话形态。数值取自真实会话的量级并向"更重"放大。
func normalSamples(t *testing.T) []burnSample {
	base := steady("steady_1h", 6, 3600, 6, 2000) // ≈333 tok/s，真实会话为 ≈443 tok/s
	// 4 个并发子智能体各 ≈1000 tok/s，持续 10 分钟
	var fleet burnSample
	fleet.id, fleet.horizon = "subagents_x4_10min", 600
	for a := 0; a < 4; a++ {
		for at := 1.0 + float64(a)*0.25; at < 600; at += 2 {
			fleet.events = append(fleet.events, burnEvent{at: at, tokens: 2000})
		}
	}
	sort.Slice(fleet.events, func(i, j int) bool { return fleet.events[i].at < fleet.events[j].at })
	return []burnSample{
		loadRealSession(t),
		base,
		withEvent(base, "steady_plus_compaction_miss_60k", 1800.5, 60000),
		withEvent(base, "steady_plus_cold_100k", 1800.5, 100000),
		withEvent(steady("idle", 0, 1, 1, 0), "cold_start_100k_first_call", 1, 100000),
		fleet,
	}
}

// runawaySamples 失控形态：持续高速且大量未命中缓存。
func runawaySamples() []burnSample {
	return []burnSample{
		steady("runaway_30k_tps", 0.34, 120, 0.34, 10200), // 约 30k tok/s
		steady("runaway_12k_tps", 0.5, 300, 0.5, 6000),    // 约 12k tok/s
	}
}

// replay 按秒推进，返回峰值 ema5s/ema30s 与首次越线时刻（-1 = 未越线）。
func replay(s burnSample, over func(*metrics.TokenBurnRate) bool) (peak5, peak30, firstOver float64) {
	start := time.Unix(1_000_000, 0)
	tbr := metrics.NewTokenBurnRateAt(start)
	firstOver = -1
	i := 0
	for sec := 1.0; sec <= s.horizon+60; sec++ {
		for i < len(s.events) && s.events[i].at <= sec {
			tbr.Add(s.events[i].tokens)
			i++
		}
		tbr.TickAt(start.Add(time.Duration(sec * float64(time.Second))))
		peak5, peak30 = max(peak5, tbr.EMA5s()), max(peak30, tbr.EMA30s())
		if firstOver < 0 && over(tbr) {
			firstOver = sec
		}
	}
	return
}

func current(tbr *metrics.TokenBurnRate) bool { return tbr.OverThrottleLine() }

// legacy 旧判据：EMA_5s > 2×冷启动基线 200。仅作对照，证明误报可复现。
func legacy(tbr *metrics.TokenBurnRate) bool { return tbr.EMA5s() > 2*200 }

func TestBurnRateEval_NormalSessionsNeverThrottle(t *testing.T) {
	for _, s := range normalSamples(t) {
		t.Run(s.id, func(t *testing.T) {
			p5, p30, first := replay(s, current)
			t.Logf("peak ema5s=%.0f ema30s=%.0f threshold=%.0f", p5, p30, metrics.ThrottleMultiplier*metrics.DefaultThrottleBaseline)
			assert.Less(t, first, 0.0, "正常会话不得越过 Stage1 触发线（t=%.0fs 越线）", first)
		})
	}
}

// 对照：旧判据在真实会话上必须误报，否则本 Eval 没有复现原缺陷。
func TestBurnRateEval_LegacyCriterionReproducesFalsePositive(t *testing.T) {
	_, _, first := replay(loadRealSession(t), legacy)
	assert.GreaterOrEqual(t, first, 0.0, "旧判据（EMA_5s>2×200）应在真实会话上误报")
}

func TestBurnRateEval_RunawayStillThrottles(t *testing.T) {
	limits := map[string]float64{"runaway_30k_tps": 10, "runaway_12k_tps": 60}
	for _, s := range runawaySamples() {
		t.Run(s.id, func(t *testing.T) {
			_, p30, first := replay(s, current)
			t.Logf("peak ema30s=%.0f first_over=%.0fs", p30, first)
			require.GreaterOrEqual(t, first, 0.0, "失控样本必须越线")
			assert.LessOrEqual(t, first, limits[s.id], "失控样本须在 %.0fs 内越线", limits[s.id])
		})
	}
}

// 失控期间学习基线不得追着失控速率跑（旧实现：callCount>=100 后基线以 0.2 速率追 EMA_30s，触发线随之抬升）。
func TestBurnRateEval_LearnedBaselineDoesNotChaseRunaway(t *testing.T) {
	s := steady("runaway_many_calls", 0.1, 300, 0.1, 3000) // 30k tok/s，>100 次调用
	_, _, first := replay(s, current)
	require.GreaterOrEqual(t, first, 0.0)
	start := time.Unix(1_000_000, 0)
	tbr := metrics.NewTokenBurnRateAt(start)
	for sec := 1; sec <= 300; sec++ {
		for k := 0; k < 10; k++ {
			tbr.Add(3000)
		}
		tbr.TickAt(start.Add(time.Duration(sec) * time.Second))
	}
	assert.True(t, tbr.OverThrottleLine(), "持续 300s 失控后仍应处于越线状态")
	assert.Less(t, tbr.BaselineP95(), 1000.0, "失控速率不得被学成基线")
}

// TestBurnRateEval_CandidateSweep 候选基线底线扫描：记录各候选的最大误报余量与失控检出时间，
// 并断言选定值落在可行区间（所有正常样本峰值 < 触发线，且失控检出达标）。数值随运行打印，供复核。
func TestBurnRateEval_CandidateSweep(t *testing.T) {
	normals := normalSamples(t)
	var normalPeak30 float64
	for _, s := range normals {
		_, p30, _ := replay(s, current)
		normalPeak30 = max(normalPeak30, p30)
	}
	t.Logf("正常样本 EMA_30s 峰值上界 = %.0f tok/s", normalPeak30)

	for _, floor := range []float64{200, 1000, 2000, 4000, 8000, 16000} {
		line := metrics.ThrottleMultiplier * floor
		detect := func(s burnSample) float64 {
			_, _, first := replay(s, func(tbr *metrics.TokenBurnRate) bool { return tbr.EMA30s() > line })
			return first
		}
		falsePos := normalPeak30 > line
		t.Logf("floor=%-6.0f line=%-6.0f 误报=%-5v runaway30k 检出=%.0fs runaway12k 检出=%.0fs",
			floor, line, falsePos, detect(runawaySamples()[0]), detect(runawaySamples()[1]))
	}

	line := metrics.ThrottleMultiplier * metrics.DefaultThrottleBaseline
	assert.Greater(t, line, normalPeak30*1.2, "选定触发线须高于正常样本峰值并留 ≥20%% 余量")
}
