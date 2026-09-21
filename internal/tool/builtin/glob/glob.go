package glob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeGlobFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args struct {
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "glob: invalid args", err)
		}
		if len(paths) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "glob: no allowed paths configured")
		}

		// 遍历所有允许路径，而非仅第一个
		var fullPaths []string
		for _, workDir := range guard.SearchRoots(ctx, allowedPaths) {
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
