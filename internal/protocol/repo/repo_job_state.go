package repo

import (
	"context"
	"time"
)

// BackgroundJobStateRepository 后台周期任务的"上次跑完"状态持久化（041_background_job_state.sql）。
// 接口由调用方（automation.IdleEvolutionScheduler）消费。
//
// @producer: internal/store/repo/repo_job_state.go
type BackgroundJobStateRepository interface {
	// GetLastRun 返回任务上次成功评估的时间；found=false 表示从未记录。
	GetLastRun(ctx context.Context, job string) (at time.Time, found bool, err error)
	// RecordRun 记录一次已完成的评估。status ∈ {success, no_work}。
	RecordRun(ctx context.Context, job, status string, at time.Time) error
}
