package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// StartMonthlyCostReport 启动月度成本报告生成后台任务。
// 按照 cron "0 0 1 * *" 每月1号 00:00 执行，生成 monthly_cost_report.md。
// db 可为 nil（降级为空报告）。
func StartMonthlyCostReport(ctx context.Context, reportDir string, db *sql.DB) {
	schedule, err := ParseCron("0 0 1 * *")
	if err != nil {
		slog.Error("cost_report: failed to parse cron", "err", err)
		return
	}

	go func() {
		for {
			now := time.Now()
			next := schedule.NextAfter(now)
			wait := next.Sub(now)

			slog.Debug("cost_report: scheduled next run", "next_run", next)

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				if err := generateCostReport(ctx, reportDir, db); err != nil {
					slog.Error("cost_report: generation failed", "err", err)
				} else {
					slog.Info("cost_report: successfully generated monthly report")
				}
			}
		}
	}()
}

// providerRate token 成本（美元/1M tokens），按主流定价估算。
var providerRate = map[string]float64{
	"anthropic": 3.0,
	"openai":    2.5,
	"deepseek":  0.27,
	"ollama":    0.0,
	"google":    1.25,
}

func generateCostReport(ctx context.Context, dir string, db *sql.DB) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	byProvider := map[string]float64{}
	byTaskType := map[string]float64{}
	bySession := map[string]float64{}
	byCallType := map[string]float64{}

	if db != nil {
		aggregateCosts(ctx, db, monthStart, monthEnd,
			byProvider, byTaskType, bySession, byCallType)
	}

	path := filepath.Join(dir, "monthly_cost_report.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	period := fmt.Sprintf("%d-%02d", now.Year(), int(now.Month())-1)
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
	return err
}

// aggregateCosts 从 events 表聚合上月 LLM 调用成本。
// 事件 payload 中包含 provider / task_type / session_id / call_type / tokens 字段。
func aggregateCosts(ctx context.Context, db *sql.DB,
	start, end time.Time,
	byProvider, byTaskType, bySession, byCallType map[string]float64,
) {
	// 从 events 表读取推理事件（topic 前缀 'inference.'）
	rows, err := db.QueryContext(ctx, `
		SELECT topic, actor, type, payload
		FROM events
		WHERE created_at >= ? AND created_at < ?
		  AND topic LIKE 'inference.%'
	`, start.UnixMicro(), end.UnixMicro())
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

		// 从 payload 解析 tokens 和 provider 信息
		tokens, provider, taskType, sessionID, callType := parseInferencePayload(payload, topic, actor, evType)
		if tokens <= 0 || provider == "" {
			continue
		}

		rate := providerRate[provider]
		cost := float64(tokens) * rate / 1_000_000.0

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
}

// parseInferencePayload 从推理事件中提取成本相关字段。
// payload 约定格式（JSON，字段均可选）：
//
//	{"provider":"deepseek","task_type":"agent.task","session_id":"...","call_type":"llm",
//	 "input_tokens":1000,"output_tokens":200}
func parseInferencePayload(payload []byte, _ /* topic */, actor, evType string) (tokens int, provider, taskType, sessionID, callType string) {
	provider = extractJSONString(payload, "provider")
	taskType = extractJSONString(payload, "task_type")
	sessionID = extractJSONString(payload, "session_id")
	callType = extractJSONString(payload, "call_type")

	inputTokens := extractJSONInt(payload, "input_tokens")
	outputTokens := extractJSONInt(payload, "output_tokens")
	tokens = inputTokens + outputTokens

	if provider == "" {
		provider = actor
	}
	if callType == "" {
		callType = evType
	}
	return
}

// extractJSONString 从 JSON 字节中提取指定 key 的字符串值（轻量实现）。
func extractJSONString(data []byte, key string) string {
	needle := `"` + key + `":"`
	start := indexOf(data, []byte(needle))
	if start < 0 {
		return ""
	}
	start += len(needle)
	end := indexOf(data[start:], []byte(`"`))
	if end < 0 {
		return ""
	}
	return string(data[start : start+end])
}

// extractJSONInt 从 JSON 字节中提取指定 key 的整数值（轻量实现）。
func extractJSONInt(data []byte, key string) int {
	needle := `"` + key + `":`
	start := indexOf(data, []byte(needle))
	if start < 0 {
		return 0
	}
	start += len(needle)
	n := 0
	for i := start; i < len(data); i++ {
		c := data[i]
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else if i > start {
			break
		}
	}
	return n
}

func indexOf(s, sep []byte) int {
	for i := 0; i <= len(s)-len(sep); i++ {
		match := true
		for j := range sep {
			if s[i+j] != sep[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
