package checkpoint_list

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/checkpoint"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type checkpointListArgs struct {
	Limit int `json:"limit"`
}

// MakeCheckpointListFn 创建 checkpoint_list 工具执行函数（仅 full 档注册）。
//
//   - dataDir: polaris 数据目录（影子 git 仓库存放处）
//   - projectDir: 被快照的项目目录
//   - retentionCount: 快照保留数（对应 checkpoint.retention_count 阈值）
func MakeCheckpointListFn(dataDir, projectDir string, retentionCount int) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		ctx, span := otel.Tracer("polaris/tool/checkpoint_list").Start(ctx, "checkpoint_list")
		defer span.End()

		var args checkpointListArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_list: invalid args", err)
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}
		if args.Limit > 100 {
			args.Limit = 100
		}
		span.SetAttributes(attribute.Int("limit", args.Limit))

		store, err := checkpoint.NewStore(dataDir, projectDir, retentionCount)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_list: open store", err)
		}

		snaps, err := store.List(ctx, args.Limit)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint_list: list", err)
		}

		type snapResult struct {
			Hash      string `json:"hash"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		results := make([]snapResult, 0, len(snaps))
		for _, s := range snaps {
			ts := ""
			if !s.Timestamp.IsZero() {
				ts = s.Timestamp.Format("2006-01-02T15:04:05Z")
			}
			results = append(results, snapResult{
				Hash:    s.Hash,
				Message: s.Message,
				// 只暴露前 8 字符方便人类阅读（完整 hash 也在里面）
				Timestamp: ts,
			})
		}

		out := map[string]any{
			"snapshots":   results,
			"count":       len(results),
			"project_dir": projectDir,
			"note":        fmt.Sprintf("共 %d 条快照，可用 checkpoint_restore 按 hash 回滚", len(results)),
		}
		return json.Marshal(out)
	}
}
