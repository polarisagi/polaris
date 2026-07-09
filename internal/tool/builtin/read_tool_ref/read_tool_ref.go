package read_tool_ref

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type readToolRefArgs struct {
	TaskID string `json:"task_id"`
	ID     string `json:"id"`
}

func MakeReadToolRefFn(vfsRoot string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readToolRefArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: invalid args", err)
		}
		if args.TaskID == "" || args.ID == "" {
			return nil, apperr.New(apperr.CodeInternal, "read_tool_ref: task_id and id are required")
		}

		if vfsRoot == "" {
			return nil, apperr.New(apperr.CodeInternal, "read_tool_ref: vfsRoot not configured")
		}

		// Security: prevent path traversal
		cleanTaskID := filepath.Clean(args.TaskID)
		cleanID := filepath.Base(args.ID)

		// Ensure cleanTaskID doesn't escape vfsRoot
		if filepath.IsAbs(cleanTaskID) || cleanTaskID == ".." || len(cleanTaskID) > 2 && cleanTaskID[:3] == "../" {
			return nil, apperr.New(apperr.CodeForbidden, "read_tool_ref: invalid task_id path")
		}

		path := filepath.Join(vfsRoot, cleanTaskID, "tool_refs", cleanID+".log")

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: file read error", err)
		}
		return data, nil
	}
}
