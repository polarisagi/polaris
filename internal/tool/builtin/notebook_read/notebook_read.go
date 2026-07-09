package notebook_read

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeNotebookReadFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeNotebookReadFn", err)
		}
		data, err := os.ReadFile(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: read failed", err)
		}
		var nb map[string]any
		if err := json.Unmarshal(data, &nb); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: parse failed", err)
		}
		cells, _ := nb["cells"].([]any)
		var out []map[string]any
		for i, c := range cells {
			cell, _ := c.(map[string]any)
			out = append(out, map[string]any{
				"index":     i,
				"cell_type": cell["cell_type"],
				"source":    cell["source"],
				"outputs":   cell["outputs"],
			})
		}
		return json.Marshal(map[string]any{"cells": out})
	}
}
