package write_file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func MakeWriteFileFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args writeFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWriteFileFn", err)
		}
		if err := guard.CheckForbiddenPath(args.Path); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWriteFileFn", err)
		}

		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if args.Append {
			flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}

		f, err := os.OpenFile(filepath.Clean(args.Path), flag, 0o600)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file", err)
		}
		defer f.Close()

		if _, err := f.WriteString(args.Content); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file: write error", err)
		}
		return []byte(`{"written":true}`), nil
	}
}
