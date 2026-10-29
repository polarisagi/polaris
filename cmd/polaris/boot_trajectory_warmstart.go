package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/learning/surprise"
	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// 学习状态 warm-start 的取数范围（ADR-0104 决策七）。
const (
	markovWarmStartSessions = 500                 // 最近 N 个有工具事件的会话
	markovWarmStartWindow   = 30 * 24 * time.Hour // 且不早于 30 天前
	warmStartReadTimeout    = 10 * time.Second    // 启动期读账本上限，超时按读失败降级
)

// trajectoryWarmStartReader 启动期从 session_trajectory 恢复学习状态所需的读取能力（HE-3 消费端接口）。
type trajectoryWarmStartReader interface {
	RecentToolRows(ctx context.Context, perTool int) ([]repo.TrajectoryRow, error)
	RecentToolSequences(ctx context.Context, maxSessions int, sinceMs int64) ([][]string, error)
}

// warmStartPolicyEvolver 把每个工具最近 window 条调用重放进 PolicyEvolver 滑动窗口。
// 读失败只 Warn：学习状态缺失不阻断启动，退化为与改造前相同的冷启动。
func warmStartPolicyEvolver(ctx context.Context, r trajectoryWarmStartReader, pe *action.PolicyEvolver) {
	if r == nil || pe == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, warmStartReadTimeout)
	defer cancel()
	rows, err := r.RecentToolRows(rctx, pe.Window())
	if err != nil {
		slog.Warn("polaris: PolicyEvolver warm-start 读取轨迹失败，冷启动", "err", err)
		return
	}
	outcomes := toolOutcomesFromRows(rows)
	pe.WarmStart(outcomes)
	slog.Info("polaris: PolicyEvolver warm-start 完成", "outcomes", len(outcomes))
}

// toolOutcomesFromRows 由轨迹行还原 ToolOutcome（行须按时间旧→新）。
// 还原不了的字段留零值：Params 恒为 nil——实时上报路径（policyEvolverOutcomeAdapter）本就不带参数，
// PolicyEvolver 也没有逻辑读它，且保留最近 window 条完整 args 会让内存随参数体积膨胀。
// tool_ok 为 NULL 的行（理论上不会出现在工具行）按失败处理，宁可低估成功率；
// latency_ms 为 NULL 按 0；Error 仅失败行取 payload.result.error。
func toolOutcomesFromRows(rows []repo.TrajectoryRow) []action.ToolOutcome {
	out := make([]action.ToolOutcome, 0, len(rows))
	for _, row := range rows {
		if row.ToolName == "" {
			continue
		}
		o := action.ToolOutcome{
			ToolName: row.ToolName,
			Success:  row.ToolOK != nil && *row.ToolOK,
		}
		if row.LatencyMs > 0 {
			o.LatencyMs = row.LatencyMs
		}
		if !o.Success {
			var p struct {
				Result map[string]any `json:"result"`
			}
			if json.Unmarshal([]byte(row.Payload), &p) == nil {
				o.Error, _ = p.Result["error"].(string)
			}
		}
		out = append(out, o)
	}
	return out
}

// warmStartSurprise 用最近会话的工具序列（按会话分组、seq 排序）预热 SurpriseCalculator 的 Markov 矩阵。
// 读失败只 Warn，矩阵保持空白，由在线积累。
func warmStartSurprise(ctx context.Context, r trajectoryWarmStartReader, calc *surprise.SurpriseCalculator) {
	if r == nil || calc == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, warmStartReadTimeout)
	defer cancel()
	since := time.Now().Add(-markovWarmStartWindow).UnixMilli()
	seqs, err := r.RecentToolSequences(rctx, markovWarmStartSessions, since)
	if err != nil {
		slog.Warn("polaris: SurpriseCalculator warm-start 读取轨迹失败，矩阵冷启动", "err", err)
		return
	}
	m := surprise.NewMarkovMatrixFromSequences(seqs)
	calc.WithMarkovMatrix(m)
	slog.Info("polaris: SurpriseCalculator Markov warm-start 完成", "sessions", len(seqs), "transitions", m.TotalTransitions())
}
