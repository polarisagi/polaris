package todo_write

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeTodoWriteFn(allowedPaths []string, mu *sync.Mutex) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Todos []string `json:"todos"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_write: invalid args", err)
		}
		path, err := guard.GetTodoPath(allowedPaths)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeTodoWriteFn", err)
		}
		mu.Lock()
		defer mu.Unlock()
		data, _ := json.MarshalIndent(args.Todos, "", "  ")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_write: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}
