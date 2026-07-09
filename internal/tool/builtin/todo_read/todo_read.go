package todo_read

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeTodoReadFn(allowedPaths []string, mu *sync.Mutex) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		path, err := guard.GetTodoPath(allowedPaths)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeTodoReadFn", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return []byte(`{"todos":[]}`), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_read: read failed", err)
		}
		var todos []string
		if err := json.Unmarshal(data, &todos); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_read: parse failed", err)
		}
		return json.Marshal(map[string]any{"todos": todos})
	}
}
