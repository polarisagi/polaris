package job_kill

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type jobKillArgs struct {
	JobID string `json:"job_id"`
}

// MakeJobKillFn 创建 job_kill 工具执行函数。
func MakeJobKillFn(jm *bash.JobManager) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		manager := jm
		if manager == nil {
			manager = bash.GetDefaultJobManager()
		}

		var args jobKillArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "job_kill: invalid args", err)
		}
		if args.JobID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "job_kill: job_id is required")
		}

		meta, err := manager.KillJob(args.JobID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "job_kill: kill job", err)
		}

		return json.Marshal(meta)
	}
}
