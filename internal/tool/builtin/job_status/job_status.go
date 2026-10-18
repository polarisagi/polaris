package job_status

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type jobStatusArgs struct {
	JobID     string `json:"job_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// MakeJobStatusFn 创建 job_status 工具执行函数。
func MakeJobStatusFn(jm *bash.JobManager) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		manager := jm
		if manager == nil {
			manager = bash.GetDefaultJobManager()
		}

		var args jobStatusArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "job_status: invalid args", err)
			}
		}

		if args.JobID != "" {
			meta, err := manager.GetJob(args.JobID)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "job_status: get job", err)
			}
			return json.Marshal(meta)
		}

		sessID := args.SessionID
		if sessID == "" {
			sessID, _ = guard.SessionIDFromCtx(ctx)
		}

		jobs, err := manager.ListJobs(sessID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "job_status: list jobs", err)
		}
		return json.Marshal(map[string]any{"jobs": jobs})
	}
}
