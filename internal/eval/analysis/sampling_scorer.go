// sampling_scorer.go — 连续采样监控的写侧数据源（M12 §9）。
//
// 2026-07-14 补齐：ContinuousSamplingMonitor.RecordSample 此前全仓零生产调用点
// （仅测试调用），读侧（GetL3Threshold/Start/CheckDegradation）已完整接入但
// 从未真正拿到过样本，窗口永远为空、基线永远建立不起来。按 M12-Eval-Harness.md
// §9 设计补齐写侧：对生产回复流量按 samplingRate（1%）抽样，用 LLM Judge
// （与 ShadowExecutor.scoreShadow / RunnerImpl Level4LLMJudge 同类模式，
// 但输出连续分数而非布尔 pass/fail）对"用户问题×AI回复"打一个 [0,1] 质量分，
// 回灌 RecordSample。此决策已与用户确认（持续 LLM 调用开销 + 用户对话内容
// 发给评判模型的隐私面），选择"按文档全量实现"。
package analysis

import (
	"context"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// scoreJudgeTimeout 单次打分 LLM 调用超时（独立于请求 ctx，异步执行）。
const scoreJudgeTimeout = 20 * time.Second

// MaybeSampleAndScore 按 samplingRate（1%）概率对一轮生产问答异步打分并回灌
// RecordSample。非抽中样本直接返回（零开销，不占用请求路径时间）；抽中样本
// 在独立 goroutine 中执行 LLM Judge 调用，不阻塞调用方、不共享调用方 ctx
// 生命周期（HTTP 请求可能在 LLM 判分完成前已经结束）。
//
// provider 为 nil（未配置可用 Provider）或 response 为空（无有效回复可评）时
// 直接跳过，不计入采样窗口，避免污染退化基线。
func (m *ContinuousSamplingMonitor) MaybeSampleAndScore(provider protocol.Provider, sessionID, query, response string) {
	if provider == nil || strings.TrimSpace(response) == "" {
		return
	}
	if rand.Float64() > samplingRate { //nolint:gosec // 非密码学用途，仅做流量采样门控
		return
	}
	concurrent.SafeGo(context.Background(), "eval.analysis.sampling_monitor_score", func(bgCtx context.Context) {
		scoreCtx, cancel := context.WithTimeout(bgCtx, scoreJudgeTimeout)
		defer cancel()
		score, err := judgeReplyQuality(scoreCtx, provider, query, response)
		if err != nil {
			slog.Warn("sampling_monitor: judge scoring failed, sample dropped", "session", sessionID, "err", err)
			return
		}
		m.RecordSample(score)
		if m.prmCollector != nil {
			m.prmCollector.Add(protocol.TrainingSample{Prompt: query, Completion: response, Reward: score})
		}
	})
}

// judgeReplyQuality 用 LLM Judge 对单轮问答给出 [0,1] 质量分（相关性+准确性+
// 完整性综合评估）。与 RunnerImpl 的 Level4LLMJudge（runner_eval.go）/
// ShadowExecutor.scoreShadow（shadow_executor.go）是同一 safecall.Infer 调用
// 模式，但那两处解析结构化 JudgeResult 返回布尔 pass/fail，服务于"是否达标"
// 的判定场景；这里服务于"退化趋势"场景，需要的是连续可比较的数值而非布尔值，
// 因此走独立的极简数字解析，不复用 judge_schema.go 的结构化 schema。
func judgeReplyQuality(ctx context.Context, provider protocol.Provider, query, response string) (float64, error) {
	resp, err := safecall.Infer(ctx, provider, []types.Message{
		{
			Role: "system",
			Content: "你是质量评审员。给出一个 0.00 到 1.00 之间的小数，代表 AI 回复相对于" +
				"用户问题的整体质量（相关性、准确性、完整性的综合评估）。只输出这个数字，" +
				"不要输出任何其它文字或符号。",
		},
		{Role: "user", Content: "用户问题：\n" + query + "\n\nAI 回复：\n" + response},
	}, types.WithModel("standard"))
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "sampling_monitor: judge inference failed", err)
	}

	raw := extractLeadingFloat(strings.TrimSpace(resp.Content))
	score, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "sampling_monitor: judge score unparsable: "+raw, err)
	}
	switch {
	case score < 0:
		score = 0
	case score > 1:
		score = 1
	}
	return score, nil
}

// extractLeadingFloat 从模型输出中截取首个数字片段，容错模型偶尔附加多余
// 文本（如 "0.85" 前后带空格/句号，或误加 "分数：" 等前缀）的情况；也兼容
// 判分越界时模型误输出的负数（如 "-0.3"），交给调用方 clamp 到 [0,1]。
func extractLeadingFloat(s string) string {
	start, end := -1, -1
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			if start == -1 {
				start = i
				if i > 0 && s[i-1] == '-' {
					start = i - 1
				}
			}
			end = i + 1
			continue
		}
		if start != -1 {
			break
		}
	}
	if start == -1 {
		return s
	}
	return s[start:end]
}
