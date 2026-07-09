package cronadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

func finalizeWorktreeChanges(ctx context.Context, ca *CronAdmin, a *automation, runID, wtDir, branchName string, wtManager WorktreeManager, status, errMsg *string) {
	hasChanges, diffSummary, err := wtManager.CommitChanges(ctx, wtDir, branchName)
	if err != nil {
		slog.Error("automation: commit worktree changes failed", "err", err)
		*status = "error"
		*errMsg = "commit worktree changes failed: " + err.Error()
		return
	}
	if !hasChanges {
		return
	}

	approved := true
	if ca.HITLGateway != nil {
		resp, hitlErr := ca.HITLGateway.Prompt(ctx, types.HITLPrompt{
			ID:             "worktree-push:" + runID,
			CheckpointType: "worktree_auto_push",
			PromptText: fmt.Sprintf("自动化任务 [%s] 生成了代码改动，请审核后批准推送到远端分支 %s：\n\n%s",
				a.Name, branchName, diffSummary),
			RiskLevel:  3,
			TaintLevel: types.TaintHigh,
			DeadlineNs: time.Now().Add(24 * time.Hour).UnixNano(),
		})
		approved = hitlErr == nil && resp != nil && resp.Approved
		if !approved {
			reason := "HITL 超时或拒绝"
			if resp != nil {
				reason = resp.Reason
			}
			slog.Warn("automation: worktree push withheld", "branch", branchName, "reason", reason)
		}
	}
	if !approved {
		return
	}

	if err := wtManager.PushBranch(ctx, wtDir, branchName); err != nil {
		slog.Error("automation: push worktree branch failed", "err", err)
		*status = "error"
		*errMsg = "push worktree branch failed: " + err.Error()
		return
	}
	slog.Info("automation: worktree changes committed and pushed", "branch", branchName)

	prTitle := fmt.Sprintf("automation: %s (%s)", a.Name, runID)
	prBody := fmt.Sprintf("由自动化任务 [%s] 生成，触发 run_id=%s。\n\n%s", a.Name, runID, diffSummary)
	if err := wtManager.CreatePullRequest(ctx, branchName, prTitle, prBody); err != nil {
		// PR 创建失败不影响本次自动化的成功状态（push 已完成，属于非致命的便利性增强失败）。
		slog.Warn("automation: create pull request failed (non-fatal)", "branch", branchName, "err", err)
	}
}

// executeAutomation 创建 run 记录、调用 Agent 执行、更新状态。
// 返回 runID，异步执行不阻塞调用方。
//
//nolint:gocyclo,funlen
func (ca *CronAdmin) executeAutomation(ctx context.Context, a *automation, trigger string) string {
	runID := newRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. 生成 session ID
	sessionID := NewSessionID()

	// 2. 写 run 记录（running 状态）
	if err := ca.AutomationRepo.CreateRun(ctx, repo.AutomationRunRow{
		ID:           runID,
		AutomationID: a.ID,
		Status:       "pending",
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Warn("automation: insert run failed", "run", runID, "err", err)
	}

	// 3. 更新 automations 执行状态
	nextRun := CalcNextRun(a.CronSchedule, now)
	if err := ca.AutomationRepo.UpdateAutomationStatusAndSchedule(ctx, a.ID, "running", now, nextRun); err != nil {
		slog.Warn("automation: update status failed", "id", a.ID, "err", err)
	}

	// 2026-07-08 系统最优加固：此前是裸 `go func(){}()`，任一 panic（ca.Chat/
	// ca.Registry 等硬依赖为 nil、Provider 流式推理异常、worktree 操作异常等）
	// 会直接终止整个进程；HTTP 路径有 withMiddleware 的 PanicRecovery 兜底
	// （server_lifecycle.go:190），但本 goroutine 无论从 cron/event/webhook
	// 后台触发还是从 manual HTTP 触发都会脱离调用方 goroutine 独立执行，均不
	// 受 HTTP 中间件保护。改用 pkg/concurrent.SafeGo 统一接入（ADR-0029 §H
	// "SafeGo 全量迁移"未覆盖到本文件，是覆盖遗漏而非有意延后）。
	concurrent.SafeGo(context.Background(), "cronadmin.executeAutomation.worker", func(context.Context) {
		// 动态映射超时
		timeout := 15 * time.Minute
		switch a.ReasoningEffort {
		case "low":
			timeout = 5 * time.Minute
		case "medium":
			timeout = 15 * time.Minute
		case "high":
			timeout = 30 * time.Minute
		case "ultra":
			timeout = 60 * time.Minute
		}

		bgCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// 注入上下文（Sandbox Level / Cedar Rules）
		bgCtx = context.WithValue(bgCtx, ctxKeySandboxLevel, a.SandboxLevel)
		bgCtx = context.WithValue(bgCtx, ctxKeyCedarRules, a.CedarRulesJSON)

		status := "ok"
		errMsg := ""
		finishedAt := ""

		var wtManager WorktreeManager
		var wtDir, branchName string
		if a.EnvType == "worktree" {
			wtManager = ca.NewWorktreeManager(a.WorkingDir, filepath.Join(os.TempDir(), "polaris-worktrees"))
			var wtErr error
			wtDir, branchName, wtErr = wtManager.PrepareWorktree(bgCtx, runID)
			if wtErr != nil {
				status = "error"
				errMsg = "worktree setup failed: " + wtErr.Error()
				slog.Error("automation: worktree setup failed", "err", wtErr)
				return
			}
			a.WorkingDir = wtDir // Temporarily override WorkingDir for this execution
		}

		defer func() {
			if wtManager != nil {
				defer wtManager.Cleanup(wtDir)

				if status == "ok" {
					finalizeWorktreeChanges(context.Background(), ca, a, runID, wtDir, branchName, wtManager, &status, &errMsg)
				}
			}

			finishedAt = time.Now().UTC().Format(time.RFC3339)
			// 更新 run 记录
			if err := ca.AutomationRepo.UpdateRunStatus(ctx, runID, status, errMsg, time.Now().UTC().Format(time.RFC3339), 0); err != nil {
				slog.Warn("automation: update run failed", "run", runID, "err", err)
			}

			// 更新 automations 统计（含电路断路器，见 updateAutomationStats）
			ca.updateAutomationStats(a.ID, status, errMsg, finishedAt)
		}()

		// 准备 Intent
		userMessage := a.Prompt
		if a.WorkingDir != "" {
			userMessage = "[工作目录: " + a.WorkingDir + "]\n\n" + a.Prompt
		}

		intent := types.Intent{
			Query:      userMessage,
			WorkingDir: a.WorkingDir,
		}

		if ca.HITLGateway != nil && a.RequiresHITL {
			// 更新状态为 suspended，等待审批
			_ = ca.AutomationRepo.UpdateRunStatus(bgCtx, runID, "suspended", "", "", 0)
			_ = ca.AutomationRepo.UpdateAutomationStatus(bgCtx, a.ID, "suspended")

			prompt := types.HITLPrompt{
				ID:             "automation:" + runID,
				CheckpointType: "automation_pre_run",
				PromptText:     fmt.Sprintf("自动化任务 [%s] 即将执行，触发方式: %s", a.Name, trigger),
				RiskLevel:      a.RiskLevel,
				DeadlineNs:     time.Now().Add(10 * time.Minute).UnixNano(),
				TaintLevel:     types.TaintLevel(a.SandboxLevel),
			}

			resp, hitlErr := ca.HITLGateway.Prompt(bgCtx, prompt)
			if hitlErr != nil || (resp != nil && !resp.Approved) {
				reason := "HITL 超时或拒绝"
				if resp != nil {
					reason = resp.Reason
				}
				status = "error"
				errMsg = "HITL 拒绝: " + reason
				return
			}
			// 审批通过，继续执行
			_ = ca.AutomationRepo.UpdateRunStatus(bgCtx, runID, "running", "", "", 0)
			_ = ca.AutomationRepo.UpdateAutomationStatus(bgCtx, a.ID, "running")
		}

		res, err := ca.AgentPool.AcquireHeadless(bgCtx, intent)
		if err != nil {
			status = "error"
			errMsg = "agent headless execution failed: " + err.Error()
			return
		}

		reply := res.Output
		latencyMs := res.LatencyMs

		if err := ca.Chat.SaveMessage(bgCtx, sessionID, "assistant", reply, "", "", latencyMs); err != nil {
			slog.Warn("automation: saveMessage assistant failed", "err", err)
		}
		_ = ca.Chat.UpdateSessionTitle(bgCtx, sessionID, a.Name)

		// 处理 result_action
		if chID, ok := strings.CutPrefix(a.ResultAction, "channel:"); ok {
			// 向 Channel 发送消息：原 channelType="" 导致 SendReply 走 default 分支静默丢弃。
			// 须先从 DB 读取 channel 的 type 和 config_json，才能正确分发。
			var chType, cfgJSON string
			if qErr := ca.DB.QueryRowContext(bgCtx,
				`SELECT type, config_json FROM channels WHERE id=?`, chID).
				Scan(&chType, &cfgJSON); qErr == nil {
				var cfg map[string]any
				_ = json.Unmarshal([]byte(cfgJSON), &cfg)
				ca.ChannelMgr.SendReply(bgCtx, chType, chID, cfg, cadapter.Message{ChatID: ""}, reply)
			} else {
				slog.Warn("automation: channel not found for result_action",
					"automation_id", a.ID, "channel_id", chID, "err", qErr)
			}
		}
	})

	return runID
}

// updateAutomationStats 更新 automations 表统计字段，含 Gap-C 电路断路器逻辑。
// 连续 CircuitBreakThreshold 次 error → 置 circuit_open=1，cronTick 跳过该任务。
// status=ok 时清零 failure_count 和 circuit_open（断路自愈）。
func (ca *CronAdmin) updateAutomationStats(automationID, status, errMsg, finishedAt string) {
	bg := context.Background()
	circuitOpen, err := ca.AutomationRepo.UpdateAutomationStats(bg, automationID, status, errMsg, finishedAt, CircuitBreakThreshold)
	if err != nil {
		slog.Warn("automation: update stats failed", "id", automationID, "err", err)
		return
	}
	if circuitOpen == 1 {
		slog.Warn("automation: circuit opened — consecutive failures exceeded threshold",
			"id", automationID, "threshold", CircuitBreakThreshold)
	}
}
