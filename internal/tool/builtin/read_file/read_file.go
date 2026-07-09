package read_file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type readFileArgs struct {
	Path string `json:"path"`
}

func MakeReadFileFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeReadFileFn", err)
		}

		data, err := os.ReadFile(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file", err)
		}
		return data, nil
	}
}
