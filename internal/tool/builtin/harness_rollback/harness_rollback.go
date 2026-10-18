package harness_rollback

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type harnessRollbackArgs struct {
	Version string `json:"version"`
}

// AuditTrail 审计接口。
type AuditTrail interface {
	Record(ctx context.Context, actor, action, target string, metadata map[string]any) error
}

// MakeHarnessRollbackFn 创建 harness_rollback 执行函数。
func MakeHarnessRollbackFn(mgr protocol.HarnessOverlayManager, audit AuditTrail) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if mgr == nil {
			return nil, apperr.New(apperr.CodeInternal, "harness_rollback: overlay manager not configured")
		}

		var args harnessRollbackArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "harness_rollback: invalid args", err)
		}
		if args.Version == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "harness_rollback: version is required")
		}

		if err := mgr.Rollback(ctx, args.Version); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "harness_rollback: rollback failed", err)
		}

		if audit != nil {
			if aErr := audit.Record(ctx, "polaris", "harness_overlay_rollback", args.Version, map[string]any{
				"version": args.Version,
			}); aErr != nil {
				slog.WarnContext(ctx, "harness_rollback: record audit failed", "err", aErr)
			}
		}

		res := map[string]any{
			"success": true,
			"version": args.Version,
		}
		b, err := json.Marshal(res)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "harness_rollback: marshal response", err)
		}
		return b, nil
	}
}
