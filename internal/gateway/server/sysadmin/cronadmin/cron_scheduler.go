package cronadmin

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// StartCronRunner 从 cron_runner.go 拆出（R7 文件行数治理，2026-07-07）：
// 本文件只负责"何时触发"的调度轮询（cron 时间到达 / 内部事件到达），
// 具体的"如何执行一次 automation"逻辑保留在 cron_runner.go 的 executeAutomation。
//
// 2026-07-08 系统最优加固（复核 code-quality-remediation-verification-20260707.md
// Phase 4 遗留项时发现）：cronTick/eventTick 及其派生的 executeAutomation 此前均为
// 裸 `go func(){}()`，任一 panic（ca.Chat/ca.Registry 等硬依赖为 nil、Provider
// 流式推理异常等）会直接终止整个进程——不同于 HTTP 路径有 withMiddleware 的
// PanicRecovery 兜底（server_lifecycle.go:190，单个 handler panic 只返回 500），
// 后台调度 goroutine 完全未接入 ADR-0029 §H "SafeGo 全量迁移"（该 ADR 迁移
// embedding_batcher/channel adapter 时 cronadmin 尚未从 sysadmin 拆分为独立子包，
// 是覆盖遗漏而非有意延后）。现改用 pkg/concurrent.SafeGo 统一接入：一次 tick 或
// 一次 automation 执行 panic 时只记录堆栈（slog.Error）+ 计数
// （concurrent.PanicTotal），不影响调度器自身存活与后续 tick。
func (ca *CronAdmin) StartCronRunner(ctx context.Context) {
	concurrent.SafeGo(ctx, "cronadmin.scheduler_loop", func(ctx context.Context) {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				concurrent.SafeGo(ctx, "cronadmin.cronTick", func(tickCtx context.Context) { ca.cronTick(tickCtx) })
				concurrent.SafeGo(ctx, "cronadmin.eventTick", func(tickCtx context.Context) { ca.eventTick(tickCtx) })
			}
		}
	})
}

// cronTick 扫描 next_run_at <= NOW() 的任务并触发执行。
//
//nolint:unused
func (ca *CronAdmin) cronTick(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := ca.DB.QueryContext(ctx, `
			SELECT id, name, prompt, trigger_type, cron_schedule,
			       working_dir, env_type, reasoning_effort, result_action, sandbox_level, cedar_rules_json,
			       requires_hitl, risk_level
			FROM automations
		WHERE enabled=1
		  AND circuit_open=0
		  AND (trigger_type='cron' OR trigger_type='both')
		  AND cron_schedule != ''
		  AND (next_run_at = '' OR next_run_at <= ?)
		  AND last_run_status != 'running'`,
		now)
	if err != nil {
		slog.Warn("cronTick: query failed", "err", err)
		return
	}
	defer rows.Close()

	var due []automation
	for rows.Next() {
		var a automation
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule,
			&a.WorkingDir, &a.EnvType, &a.ReasoningEffort, &a.ResultAction, &a.SandboxLevel, &a.CedarRulesJSON,
			&a.RequiresHITL, &a.RiskLevel,
		); err != nil {
			continue
		}
		due = append(due, a)
	}
	rows.Close()

	for i := range due {
		a := &due[i]
		concurrent.SafeGo(ctx, "cronadmin.executeAutomation.cron", func(execCtx context.Context) { ca.executeAutomation(execCtx, a, "cron") })
	}

	// 同批次触发到期工作流
	ca.CronTickWorkflows(ctx)
}

// eventTick 处理内部事件触发的 automation (trigger_type='event' or 'both').
//
//nolint:unused
func (ca *CronAdmin) eventTick(ctx context.Context) {
	// 提取当前增量事件 (since lastEventOffset)
	rows, err := ca.DB.QueryContext(ctx, `
		SELECT offset, topic, type, payload
		FROM events
		WHERE offset > ? ORDER BY offset ASC
	`, ca.LastEventOffset)
	if err != nil {
		slog.Warn("eventTick: query events failed", "err", err)
		return
	}
	defer rows.Close()

	var events []struct {
		Offset  int64
		Topic   string
		Type    string
		Payload string
	}
	maxOffset := ca.LastEventOffset
	for rows.Next() {
		var ev struct {
			Offset  int64
			Topic   string
			Type    string
			Payload string
		}
		if err := rows.Scan(&ev.Offset, &ev.Topic, &ev.Type, &ev.Payload); err == nil {
			events = append(events, ev)
			if ev.Offset > maxOffset {
				maxOffset = ev.Offset
			}
		}
	}
	rows.Close()

	if len(events) == 0 {
		return
	}

	// 查找配置为 event 触发的 automations
	aRows, err := ca.DB.QueryContext(ctx, `
			SELECT id, name, prompt, trigger_type, cron_schedule,
			       working_dir, env_type, reasoning_effort, result_action, sandbox_level, cedar_rules_json, event_filter,
			       requires_hitl, risk_level
			FROM automations
		WHERE enabled=1
		  AND circuit_open=0
		  AND (trigger_type='event' OR trigger_type='both')
		  AND event_filter != '' AND event_filter != '{}'
		  AND last_run_status != 'running'
	`)
	if err != nil {
		slog.Warn("eventTick: query automations failed", "err", err)
		return
	}
	defer aRows.Close()

	var autos []automation
	for aRows.Next() {
		var a automation
		if err := aRows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule,
			&a.WorkingDir, &a.EnvType, &a.ReasoningEffort, &a.ResultAction, &a.SandboxLevel, &a.CedarRulesJSON, &a.EventFilter,
			&a.RequiresHITL, &a.RiskLevel,
		); err == nil {
			autos = append(autos, a)
		}
	}
	aRows.Close()

	for _, ev := range events {
		for i := range autos {
			a := &autos[i]
			if matchEventFilter(a.EventFilter, ev.Topic, ev.Type, ev.Payload) {
				concurrent.SafeGo(ctx, "cronadmin.executeAutomation.event", func(execCtx context.Context) { ca.executeAutomation(execCtx, a, "event") })
			}
		}
	}

	ca.LastEventOffset = maxOffset
}
