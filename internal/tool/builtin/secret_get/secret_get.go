package secret_get

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// NamedSecretReader 按名称读取凭据的接口（consumer-side 定义，防止反向依赖具体存储实现）。
// 实现方负责解密：返回的 value 是明文。
type NamedSecretReader interface {
	GetSecret(ctx context.Context, name string) (value string, taintLevel types.TaintLevel, err error)
}

type secretGetArgs struct {
	Name string `json:"name"`
}

// MakeSecretGetFn 创建 secret_get 工具执行函数（仅 full 档注册）。
//
//   - reader: 凭据读取接口（可为 nil，nil 时所有调用返回错误而非 panic）
//   - maxPerTurn: 单回合最多读取条数（对应 secret_get.max_per_turn 阈值）
//
// 回合计数通过闭包内 atomic.Int64 实现；每次 polaris 新建回合（goroutine 级）时
// 应重置计数器（通过 ResetTurnCounter 暴露给上层调度器）。
func MakeSecretGetFn(reader NamedSecretReader, maxPerTurn int) (sandbox.InProcessFn, *TurnCounter) {
	counter := &TurnCounter{}
	fn := func(ctx context.Context, input []byte) ([]byte, error) {
		ctx, span := otel.Tracer("polaris/tool/secret_get").Start(ctx, "secret_get")
		defer span.End()

		if reader == nil {
			return nil, apperr.New(apperr.CodeInternal, "secret_get: NamedSecretReader not configured (full profile required)")
		}

		var args secretGetArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "secret_get: invalid args", err)
		}
		if args.Name == "" {
			return nil, apperr.New(apperr.CodeInternal, "secret_get: name is required")
		}
		span.SetAttributes(attribute.String("secret_name", args.Name))

		// 回合内限速：防止批量读取凭据（ADR-0115 WP1 行为矩阵）
		n := counter.Inc()
		if maxPerTurn > 0 && int(n) > maxPerTurn {
			return nil, apperr.New(apperr.CodeForbidden,
				"secret_get: 本回合已达凭据读取上限（secret_get.max_per_turn），防止批量泄露")
		}

		value, taintLevel, err := reader.GetSecret(ctx, args.Name)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "secret_get: read secret", err)
		}
		_ = taintLevel // 凭据值标记 TaintHigh，由上层 tool 层污点系统处理

		result := map[string]any{
			"name":  args.Name,
			"value": value,
			// 不暴露 taintLevel 给模型，避免提示模型绕过污点规则
		}
		return json.Marshal(result)
	}
	return fn, counter
}

// TurnCounter 回合内凭据读取计数器。
// 上层调度器在每次新回合开始时调用 Reset。
type TurnCounter struct {
	n atomic.Int64
}

// Inc 自增并返回新计数。
func (c *TurnCounter) Inc() int64 { return c.n.Add(1) }

// Reset 重置回合计数（新回合开始时调用）。
func (c *TurnCounter) Reset() { c.n.Store(0) }

// Count 返回当前计数。
func (c *TurnCounter) Count() int64 { return c.n.Load() }
