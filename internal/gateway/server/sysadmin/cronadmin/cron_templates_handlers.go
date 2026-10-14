package cronadmin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
)

// ─── GET /v1/automation-templates ─────────────────────────────────────────────
//
// 从 cron_handlers.go 拆出（R7 文件行数治理，2026-07-07）：本文件收录"模板发现 +
// Webhook 触发"两类与 /v1/automations CRUD 语义不同的入口，逻辑不变。

func (ca *CronAdmin) HandleListAutomationTemplates(w http.ResponseWriter, r *http.Request) {
	filterSource := r.URL.Query().Get("source")
	filterTag := r.URL.Query().Get("tag")

	srcs := loadSources()
	var all []automationTemplate

	for _, src := range srcs {
		if !src.Enabled {
			continue
		}
		if filterSource != "" && src.ID != filterSource {
			continue
		}
		var tpls []automationTemplate
		switch src.Type {
		case "local":
			tpls = loadLocalTemplates(src.Path)
		case "remote":
			if src.URL != "" {
				tpls = ca.fetchRemoteTemplates(src)
			}
		}
		all = append(all, tpls...)
	}

	// 加载已启用插件声明的伴生任务模板（T10：安装 ≠ 启用）
	if ca.ExtRepo != nil {
		all = append(all, ca.loadPluginTemplates(r.Context(), filterSource)...)
	}

	// 无有效来源时 fallback 到内置模板（从 embed.FS 读取，不依赖工作目录）
	if len(all) == 0 && filterSource == "" {
		all = loadEmbeddedTemplates("automations/templates")
	}

	// 标签过滤
	if filterTag != "" {
		var filtered []automationTemplate
		for _, t := range all {
			if slices.Contains(t.Tags, filterTag) {
				filtered = append(filtered, t)
			}
		}
		all = filtered
	}

	if all == nil {
		all = []automationTemplate{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"templates": all}) //nolint:errcheck
}

func (ca *CronAdmin) loadPluginTemplates(ctx context.Context, filterSource string) []automationTemplate {
	if ca.ExtRepo == nil {
		return nil
	}
	plugins, err := ca.ExtRepo.ListPlugins(ctx)
	if err != nil {
		slog.Warn("automation-templates: list plugins failed", "err", err)
		return nil
	}
	var tpls []automationTemplate
	for _, p := range plugins {
		if !p.Enabled {
			continue
		}
		sourceID := "plugin:" + p.ID
		if filterSource != "" && filterSource != sourceID {
			continue
		}
		if p.Manifest == "" {
			continue
		}
		var plug pluginspec.Plugin
		if err := json.Unmarshal([]byte(p.Manifest), &plug); err != nil {
			continue
		}
		for _, task := range plug.ScheduledTasks {
			t := automationTemplate{
				Name:         task.Name,
				Description:  task.Description,
				Prompt:       task.Prompt,
				TriggerType:  "cron",
				CronSchedule: task.Schedule,
				Source:       sourceID,
				Tags:         []string{"plugin", plug.Name},
				Kind:         task.Kind,
				Pipeline:     task.Pipeline,
			}
			tpls = append(tpls, t)
		}
	}
	return tpls
}

// ─── Webhook 触发 ──────────────────────────────────────────────────────────────

func (ca *CronAdmin) TriggerWebhookAutomations(ctx context.Context, channelID, text string) {
	// GD-9-001 复核修复：改走 AutomationRepo.ListWebhookAutomations，不再由
	// Gateway 层直接拼接执行 SQL。
	rows, err := ca.AutomationRepo.ListWebhookAutomations(ctx, channelID)
	if err != nil {
		return
	}

	due := make([]automation, 0, len(rows))
	for _, row := range rows {
		due = append(due, automationFromRow(row))
	}

	for i := range due {
		a := &due[i]
		originalPrompt := a.Prompt
		if text != "" {
			a.Prompt = a.Prompt + "\n[Webhook Payload]:\n" + text
		}
		ca.executeAutomation(ctx, a, "webhook")
		a.Prompt = originalPrompt
	}
}
