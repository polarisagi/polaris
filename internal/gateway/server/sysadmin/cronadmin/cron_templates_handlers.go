package cronadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
)

// ─── GET /v1/automation-templates ─────────────────────────────────────────────
//
// 从 cron_handlers.go 拆出（R7 文件行数治理，2026-07-07）：本文件收录"模板发现 +
// Webhook 触发"两类与 /v1/automations CRUD 语义不同的入口，逻辑不变。

//nolint:unused
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

// ─── Webhook 触发 ──────────────────────────────────────────────────────────────

func (ca *CronAdmin) TriggerWebhookAutomations(ctx context.Context, channelID, text string) {
	rows, err := ca.DB.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json
		FROM automations
		WHERE enabled=1
		  AND (trigger_type='webhook' OR trigger_type='both')
		  AND channel_id=?
		  AND last_run_status != 'running'`,
		channelID)
	if err != nil {
		return
	}
	defer rows.Close()

	var due []automation
	for rows.Next() {
		var a automation
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule, &a.ChannelID,
			&a.WorkingDir, &a.ReasoningEffort, &a.ResultAction,
			&a.SandboxLevel, &a.CedarRulesJSON,
		); err != nil {
			continue
		}
		due = append(due, a)
	}
	rows.Close()

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
