package automation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
)

// StartMonthlyCostReport 启动月度成本报告生成后台任务。
// 按照 cron "0 0 1 * *" 每月1号 00:00 执行，生成 monthly_cost_report.md。
// db 可为 nil（降级为空报告）。
func StartMonthlyCostReport(ctx context.Context, reportDir string, db protocol.SQLQuerier) {
	schedule, err := ParseCron("0 0 1 * *")
	if err != nil {
		slog.Error("cost_report: failed to parse cron", "err", err)
		return
	}

	concurrent.SafeGo(ctx, "automation.cost_report.monthly_loop", func(ctx context.Context) {
		for {
			now := time.Now()
			next := schedule.NextAfter(now)
			wait := next.Sub(now)

			slog.Debug("cost_report: scheduled next run", "next_run", next)

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				reporter := NewCostReporter()
				if err := reporter.generateCostReport(ctx, reportDir, db); err != nil {
					slog.Error("cost_report: generation failed", "err", err)
				} else {
					slog.Info("cost_report: successfully generated monthly report")
				}
			}
		}
	})
}

// CostReporter 用于生成月度成本报告
type CostReporter struct {
	providerRate map[string]float64
}

// NewCostReporter 创建 CostReporter
func NewCostReporter() *CostReporter {
	return &CostReporter{
		// providerRate token 成本（美元/1M tokens），按主流定价估算。
		providerRate: map[string]float64{
			"anthropic": 3.0,
			"openai":    2.5,
			"deepseek":  0.27,
			"ollama":    0.0,
			"google":    1.25,
		},
	}
}

func (r *CostReporter) generateCostReport(ctx context.Context, dir string, db protocol.SQLQuerier) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CostReporter.generateCostReport", err)
	}

	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	byProvider := map[string]float64{}
	byTaskType := map[string]float64{}
	bySession := map[string]float64{}
	byCallType := map[string]float64{}

	if db != nil {
		r.aggregateCosts(ctx, db, monthStart, monthEnd,
			byProvider, byTaskType, bySession, byCallType)
	}

	path := filepath.Join(dir, "monthly_cost_report.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CostReporter.generateCostReport", err)
	}
	defer f.Close()

	// 用 monthStart 派生期间字符串，避免1月时 Month()-1=0 导致输出 "YYYY-00"
	period := fmt.Sprintf("%d-%02d", monthStart.Year(), int(monthStart.Month()))
	content := fmt.Sprintf("# Monthly Cost Report - %s\n\nGenerated at: %s\n\n",
		period, now.Format(time.RFC3339))

	content += "## 1. By Provider\n"
	if len(byProvider) == 0 {
		content += "- (no data)\n"
	}
	for p, cost := range byProvider {
		content += fmt.Sprintf("- %s: $%.4f\n", p, cost)
	}

	content += "\n## 2. By Task Type\n"
	if len(byTaskType) == 0 {
		content += "- (no data)\n"
	}
	for t, cost := range byTaskType {
		content += fmt.Sprintf("- %s: $%.4f\n", t, cost)
	}

	content += "\n## 3. By Session\n"
	if len(bySession) == 0 {
		content += "- (no data)\n"
	}
	for s, cost := range bySession {
		content += fmt.Sprintf("- %s: $%.4f\n", s, cost)
	}

	content += "\n## 4. By Call Type\n"
	if len(byCallType) == 0 {
		content += "- (no data)\n"
	}
	for c, cost := range byCallType {
		content += fmt.Sprintf("- %s: $%.4f\n", c, cost)
	}

	_, err = f.WriteString(content)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CostReporter.generateCostReport", err)
	}
	return nil
}

// aggregateCosts 从 events 表聚合上月 LLM 调用成本。
func (r *CostReporter) aggregateCosts(ctx context.Context, db protocol.SQLQuerier,
	start, end time.Time,
	byProvider, byTaskType, bySession, byCallType map[string]float64,
) {
	// 从 events 表读取推理事件（topic 'llm.call.recorded'）
	rows, err := db.QueryContext(ctx, `
		SELECT topic, actor, type, payload
		FROM events
		WHERE created_at >= ? AND created_at < ?
		  AND topic = 'llm.call.recorded'
	`, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var topic, actor, evType string
		var payload []byte
		if err := rows.Scan(&topic, &actor, &evType, &payload); err != nil {
			continue
		}

		tokens, provider, taskType, sessionID, callType, costUSD := parseInferencePayload(payload, topic, actor, evType)
		if tokens <= 0 || provider == "" {
			continue
		}

		// 使用记录的真实成本，如果为 0 则使用费率估算
		var cost float64
		if costUSD > 0 {
			cost = costUSD
		} else {
			rate := r.providerRate[provider]
			cost = float64(tokens) * rate / 1_000_000.0
		}

		byProvider[provider] += cost
		if taskType != "" {
			byTaskType[taskType] += cost
		}
		if sessionID != "" {
			bySession[sessionID] += cost
		}
		if callType != "" {
			byCallType[callType] += cost
		}
	}
	// F-7：迭代中途出错会表现为静默少行，必须检查 rows.Err()
	if err := rows.Err(); err != nil {
		slog.Warn("cost_report: 事件迭代异常", "err", err)
	}
}

// parseInferencePayload 从推理事件中提取成本相关字段。
func parseInferencePayload(payload []byte, _ /* topic */, actor, evType string) (tokens int, provider, taskType, sessionID, callType string, costUSD float64) {
	var pbPayload pb.LLMCallPayload
	if err := proto.Unmarshal(payload, &pbPayload); err != nil {
		return
	}

	provider = pbPayload.Provider
	tokens = int(pbPayload.InputTokens + pbPayload.OutputTokens)
	costUSD = pbPayload.CostUsd

	if provider == "" {
		provider = actor
	}
	callType = evType

	// Protobuf definition currently doesn't have task_type and session_id natively,
	// they might be in the event's actor or topic, but we'll leave them empty for now
	// or extract if we added them to proto. The user request didn't specify adding them.
	return
}
