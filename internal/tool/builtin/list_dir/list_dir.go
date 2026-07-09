package list_dir

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type listDirArgs struct {
	Path string `json:"path"`
}

func MakeListDirFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args listDirArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_dir: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeListDirFn", err)
		}

		entries, err := os.ReadDir(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_dir", err)
		}

		result := listDirResult{Entries: make([]dirEntry, 0, len(entries))}
		for _, e := range entries {
			info, _ := e.Info()
			var sz int64
			if info != nil {
				sz = info.Size()
			}
			result.Entries = append(result.Entries, dirEntry{
				Name:  e.Name(),
				IsDir: e.IsDir(),
				Size:  sz,
			})
		}
		return json.Marshal(result)
	}
}

type listDirResult struct {
	Entries []dirEntry `json:"entries"`
}

type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size_bytes"`
}
