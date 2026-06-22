package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── cron_list ────────────────────────────────────────────────────────────────

// makeCronListFn 返回 cron_list 工具实现。
// 直接读 cron_jobs 表，不绕过调度器——只读查询，并发安全（SQLite WAL 模式）。
func makeCronListFn(cronRepo protocol.CronRepository) sandbox.InProcessFn {
	return func(ctx context.Context, _ []byte) ([]byte, error) {
		rows, err := cronRepo.ListCronJobs(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_list: query failed", err)
		}

		type cronRow struct {
			ID           string  `json:"id"`
			Name         string  `json:"name"`
			Prompt       string  `json:"prompt"`
			Schedule     string  `json:"schedule"`
			Enabled      bool    `json:"enabled"`
			LastRunAt    *string `json:"last_run_at,omitempty"`
			NextRunAt    string  `json:"next_run_at"`
			FailureCount int     `json:"failure_count"`
			CircuitOpen  bool    `json:"circuit_open"`
			LastError    string  `json:"last_error,omitempty"`
			CreatedAt    string  `json:"created_at"`
		}

		var jobs []cronRow
		for _, j := range rows {
			var lastRunAt *string
			if j.LastRunAt != "" {
				s := j.LastRunAt
				lastRunAt = &s
			}
			jobs = append(jobs, cronRow{
				ID:           j.ID,
				Name:         j.Name,
				Prompt:       j.Prompt,
				Schedule:     j.Schedule,
				Enabled:      j.Enabled,
				LastRunAt:    lastRunAt,
				NextRunAt:    j.NextRunAt,
				FailureCount: j.FailureCount,
				CircuitOpen:  j.CircuitOpen,
				LastError:    j.LastError,
				CreatedAt:    j.CreatedAt,
			})
		}

		if jobs == nil {
			jobs = []cronRow{} // 序列化为 [] 而非 null
		}
		return json.Marshal(map[string]any{
			"jobs":  jobs,
			"count": len(jobs),
		})
	}
}

// ─── cron_create ──────────────────────────────────────────────────────────────

// makeCronCreateFn 返回 cron_create 工具实现。
// 写入 cron_jobs 表，调度器在下次轮询时按 schedule 重算 next_run_at 并执行。
// next_run_at 初始设为 1 分钟后，确保调度器拾取后立即可调度。
func makeCronCreateFn(cronRepo protocol.CronRepository) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Name      string `json:"name"`       // 任务名（可选，方便识别）
			Prompt    string `json:"prompt"`     // 触发时发给 Agent 的 prompt（必填）
			Schedule  string `json:"schedule"`   // 5 字段 cron 表达式（必填，如 "0 9 * * 1-5"）
			SessionID string `json:"session_id"` // 绑定会话（可选；空=每次新建会话）
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_create: invalid args", err)
		}
		if strings.TrimSpace(args.Prompt) == "" {
			return nil, apperr.New(apperr.CodeInternal, "cron_create: prompt is required")
		}
		if strings.TrimSpace(args.Schedule) == "" {
			return nil, apperr.New(apperr.CodeInternal, "cron_create: schedule is required")
		}

		// ID 用纳秒时间戳前缀保证单节点唯一性
		id := fmt.Sprintf("cron_%d", time.Now().UnixNano())
		// next_run_at 初始设为 1 分钟后；调度器轮询时按实际 cron 表达式重算
		nextRunAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)

		var sessionID string
		if args.SessionID != "" {
			sessionID = args.SessionID
		}

		err := cronRepo.CreateCronJob(ctx, types.CronJobRow{
			ID:        id,
			Name:      args.Name,
			Prompt:    args.Prompt,
			Schedule:  args.Schedule,
			SessionID: sessionID,
			Enabled:   true,
			NextRunAt: nextRunAt,
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_create: insert failed", err)
		}

		// Ensure next_run_at is populated if the repo does not support it in CreateCronJob
		_ = cronRepo.UpdateLastRun(ctx, id, "", nextRunAt)

		return json.Marshal(map[string]any{
			"id":          id,
			"name":        args.Name,
			"schedule":    args.Schedule,
			"next_run_at": nextRunAt,
		})
	}
}

// ─── cron_delete ──────────────────────────────────────────────────────────────

// makeCronDeleteFn 返回 cron_delete 工具实现。
// 按 ID 删除 cron_jobs 记录；ID 不存在时返回 CodeNotFound。
func makeCronDeleteFn(cronRepo protocol.CronRepository) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			ID string `json:"id"` // 任务 ID（必填；由 cron_list / cron_create 获取）
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_delete: invalid args", err)
		}
		if strings.TrimSpace(args.ID) == "" {
			return nil, apperr.New(apperr.CodeInternal, "cron_delete: id is required")
		}

		// To simulate the previous "job not found" behaviour, check if it exists
		job, err := cronRepo.GetCronJob(ctx, args.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_delete: get failed", err)
		}
		if job == nil {
			return nil, apperr.New(apperr.CodeNotFound,
				fmt.Sprintf("cron_delete: job %q not found", args.ID))
		}

		err = cronRepo.DeleteCronJob(ctx, args.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cron_delete: delete failed", err)
		}

		return json.Marshal(map[string]any{
			"id":      args.ID,
			"deleted": true,
		})
	}
}
