package checkpoint_restore

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/checkpoint"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type checkpointRestoreArgs struct {
	Hash string `json:"hash"`
}

// MakeCheckpointRestoreFn 创建 checkpoint_restore 工具执行函数（仅 full 档注册）。
//
// 恢复流程：
//  1. 先对当前工作树创建快照（保留恢复前状态）
//  2. git checkout <hash> -- . 把文件覆盖回目标快照状态
//
// full 档下 HITL 已由 Policy.HITLAutoApprove=true 自动放行（tool.yaml risk_level=high 会触发 HITL 检查）。
func MakeCheckpointRestoreFn(dataDir, projectDir string, retentionCount int) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		ctx, span := otel.Tracer("polaris/tool/checkpoint_restore").Start(ctx, "checkpoint_restore")
		defer span.End()

		var args checkpointRestoreArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_restore: invalid args", err)
		}
		if args.Hash == "" {
			return nil, apperr.New(apperr.CodeInternal, "checkpoint_restore: hash is required")
		}
		span.SetAttributes(
			attribute.String("hash", args.Hash),
			attribute.String("project_dir", projectDir),
		)

		store, err := checkpoint.NewStore(dataDir, projectDir, retentionCount)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_restore: open store", err)
		}

		if err := store.Restore(ctx, args.Hash); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_restore: restore", err)
		}

		result := map[string]any{
			"restored_to": args.Hash,
			"project_dir": projectDir,
			"message":     "工作目录已恢复到指定快照。恢复前状态已自动保存为新快照。",
		}
		return json.Marshal(result)
	}
}
