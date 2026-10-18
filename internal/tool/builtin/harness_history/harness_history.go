package harness_history

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type harnessHistoryArgs struct {
	Limit int `json:"limit"`
}

// MakeHarnessHistoryFn 创建 harness_history 执行函数。
func MakeHarnessHistoryFn(mgr protocol.HarnessOverlayManager) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if mgr == nil {
			return nil, apperr.New(apperr.CodeInternal, "harness_history: overlay manager not configured")
		}

		var args harnessHistoryArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "harness_history: invalid args", err)
			}
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}

		history, err := mgr.History(ctx, args.Limit)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "harness_history: query history failed", err)
		}

		res := map[string]any{
			"versions": history,
			"count":    len(history),
		}
		b, err := json.Marshal(res)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "harness_history: marshal failed", err)
		}
		return b, nil
	}
}
