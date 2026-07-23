package cronadmin

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// automationFromRow 将 repo.AutomationRow 转换为本包内部使用的 automation 结构体，
// 与 cron_handlers.go HandleListAutomations 的既有转换保持同一套字段映射。
func automationFromRow(row repo.AutomationRow) automation {
	return automation{
		ID:              row.ID,
		Name:            row.Name,
		Prompt:          row.Prompt,
		TriggerType:     row.TriggerType,
		CronSchedule:    row.CronSchedule,
		ChannelID:       row.ChannelID,
		WorkingDir:      row.WorkingDir,
		EnvType:         row.EnvType,
		ReasoningEffort: row.ReasoningEffort,
		ResultAction:    row.ResultAction,
		SandboxLevel:    row.SandboxLevel,
		CedarRulesJSON:  row.CedarRulesJSON,
		Enabled:         row.Enabled,
		RequiresHITL:    row.RequiresHITL,
		RiskLevel:       row.RiskLevel,
		LastRunAt:       row.LastRunAt,
		NextRunAt:       row.NextRunAt,
		RunCount:        row.RunCount,
		LastRunStatus:   row.LastRunStatus,
		LastRunError:    row.LastRunError,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
		EventFilter:     row.EventFilter,
	}
}

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
	// GD-9-001 复核修复：改走 AutomationRepo.ListDueAutomations，不再由 Gateway
	// 层直接拼接执行 SQL（R1.1 ctrl→svc→dao 分层）。
	rows, err := ca.AutomationRepo.ListDueAutomations(ctx, now)
	if err != nil {
		slog.Warn("cronTick: query failed", "err", err)
		return
	}

	due := make([]automation, 0, len(rows))
	for _, row := range rows {
		due = append(due, automationFromRow(row))
	}

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
	// GD-9-001 复核修复：提取当前增量事件 (since lastEventOffset)，改走
	// EventRepo.ListEventsSince，不再由 Gateway 层直接拼接执行 SQL。
	eventRows, err := ca.EventRepo.ListEventsSince(ctx, ca.LastEventOffset)
	if err != nil {
		slog.Warn("eventTick: query events failed", "err", err)
		return
	}

	events := make([]repo.EventRow, 0, len(eventRows))
	maxOffset := ca.LastEventOffset
	for _, ev := range eventRows {
		events = append(events, ev)
		if ev.Offset > maxOffset {
			maxOffset = ev.Offset
		}
	}

	if len(events) == 0 {
		return
	}

	// 查找配置为 event 触发的 automations（GD-9-001 复核修复：改走
	// AutomationRepo.ListEventAutomations）。
	autoRows, err := ca.AutomationRepo.ListEventAutomations(ctx)
	if err != nil {
		slog.Warn("eventTick: query automations failed", "err", err)
		return
	}

	autos := make([]automation, 0, len(autoRows))
	for _, row := range autoRows {
		autos = append(autos, automationFromRow(row))
	}

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
