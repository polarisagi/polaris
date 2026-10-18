package job_output

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type jobOutputArgs struct {
	JobID     string `json:"job_id"`
	TailLines int    `json:"tail_lines,omitempty"`
}

// MakeJobOutputFn 创建 job_output 工具执行函数。
func MakeJobOutputFn(jm *bash.JobManager) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		manager := jm
		if manager == nil {
			manager = bash.GetDefaultJobManager()
		}

		var args jobOutputArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "job_output: invalid args", err)
		}
		if args.JobID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "job_output: job_id is required")
		}

		tail := args.TailLines
		if tail <= 0 {
			tail = 200
		}

		output, err := manager.GetJobOutput(args.JobID, tail)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "job_output: read output", err)
		}

		meta, err := manager.GetJob(args.JobID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "job_output: get job", err)
		}

		result := map[string]any{
			"job_id":     args.JobID,
			"output":     output,
			"status":     meta.Status,
			"exit_code":  meta.ExitCode,
			"tail_lines": tail,
		}
		return json.Marshal(result)
	}
}
