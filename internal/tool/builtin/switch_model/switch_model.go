package switch_model

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type switchModelArgs struct {
	Ref      string `json:"ref"`
	Pool     string `json:"pool"`
	Thinking string `json:"thinking"`
}

// MakeSwitchModelFn 构造 switch_model 工具执行函数。
func MakeSwitchModelFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args switchModelArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "switch_model: invalid args", err)
			}
		}

		val := ctx.Value(protocol.CtxModelSwitcherKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "switch_model: ModelSwitcher not available in context")
		}

		switcher, ok := val.(protocol.ModelSwitcher)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "switch_model: invalid ModelSwitcher in context")
		}

		_, userLocked := switcher.GetModelStatus()
		if userLocked != "" {
			out := map[string]any{
				"status":  "rejected",
				"message": "已被用户在界面锁定为 " + userLocked + "，无法自选",
				"locked":  true,
			}
			return json.Marshal(out)
		}

		msg, err := switcher.SwitchModel(args.Ref, args.Pool, args.Thinking)
		if err != nil {
			out := map[string]any{
				"status":  "error",
				"message": err.Error(),
			}
			return json.Marshal(out)
		}

		out := map[string]any{
			"status":   "success",
			"message":  msg,
			"target":   args.Ref,
			"pool":     args.Pool,
			"thinking": args.Thinking,
		}
		return json.Marshal(out)
	}
}
