package glob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeGlobFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "glob: invalid args", err)
		}
		if len(allowedPaths) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "glob: no allowed paths configured")
		}

		// 遍历所有允许路径，而非仅第一个
		var fullPaths []string
		for _, workDir := range allowedPaths {
			fsys := os.DirFS(workDir)
			// os.DirFS 限定了根目录，doublestar.Glob 不会跨越边界
			matches, err := doublestar.Glob(fsys, args.Pattern)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "glob: error matching", err)
			}
			for _, m := range matches {
				fullPaths = append(fullPaths, filepath.Join(workDir, m))
			}
		}
		return json.Marshal(map[string]any{"matches": fullPaths})
	}
}
