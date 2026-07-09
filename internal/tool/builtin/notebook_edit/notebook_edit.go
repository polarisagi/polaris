package notebook_edit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeNotebookEditFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path      string `json:"path"`
			CellIndex int    `json:"cell_index"`
			Source    string `json:"source"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeNotebookEditFn", err)
		}
		cleanPath := filepath.Clean(args.Path)
		data, err := os.ReadFile(cleanPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: read failed", err)
		}
		var nb map[string]any
		if err := json.Unmarshal(data, &nb); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: parse failed", err)
		}
		cells, ok := nb["cells"].([]any)
		if !ok || args.CellIndex < 0 || args.CellIndex >= len(cells) {
			return nil, apperr.New(apperr.CodeInternal, "notebook_edit: cell index out of bounds")
		}
		cell, _ := cells[args.CellIndex].(map[string]any)

		// Jupyter source is usually array of strings or a single string
		// Convert new source to array of strings (lines)
		lines := strings.Split(args.Source, "\n")
		var sourceLines []string
		for i, l := range lines {
			if i < len(lines)-1 {
				sourceLines = append(sourceLines, l+"\n")
			} else {
				sourceLines = append(sourceLines, l)
			}
		}
		cell["source"] = sourceLines
		cells[args.CellIndex] = cell
		nb["cells"] = cells

		newData, _ := json.MarshalIndent(nb, "", "  ")
		if err := os.WriteFile(cleanPath, newData, 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}
