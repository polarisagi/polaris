package secret_get

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/internal/protocol"
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

// TurnCounter 回合内凭据读取计数器。
type TurnCounter struct {
	n atomic.Int64
}

// NewTurnCounter 创建新的回合计数器。
func NewTurnCounter() *TurnCounter {
	return &TurnCounter{}
}

// Inc 自增并返回新计数。
func (c *TurnCounter) Inc() int64 { return c.n.Add(1) }

// Reset 重置回合计数（新回合开始时调用）。
func (c *TurnCounter) Reset() { c.n.Store(0) }

// Count 返回当前计数。
func (c *TurnCounter) Count() int64 { return c.n.Load() }

type turnCounterKey struct{}

// WithTurnCounter 将回合计数器注入 context。
func WithTurnCounter(ctx context.Context, c *TurnCounter) context.Context {
	return context.WithValue(ctx, turnCounterKey{}, c)
}

// TurnCounterFromContext 从 context 中读取显式注入的回合计数器。
func TurnCounterFromContext(ctx context.Context) *TurnCounter {
	if ctx == nil {
		return nil
	}
	if c, ok := ctx.Value(turnCounterKey{}).(*TurnCounter); ok {
		return c
	}
	return nil
}

// MakeSecretGetFn 创建 secret_get 工具执行函数（仅 full 档注册）。
//
//   - reader: 凭据读取接口（可为 nil，nil 时所有调用返回错误而非 panic）
//   - maxPerTurn: 单回合最多读取条数（对应 secret_get.max_per_turn 阈值）
//
// 回合计数优先从 ctx 显式注入的 *TurnCounter 读取，若未注入则基于 sessionID+turnID
// 隔离映射；新回合自动清零，不使用单例全局计数器（R2-11）。
func MakeSecretGetFn(reader NamedSecretReader, maxPerTurn int) sandbox.InProcessFn {
	var (
		scopedMu        sync.Mutex
		scopedCounters  = make(map[string]*TurnCounter)
		fallbackCounter = &TurnCounter{}
	)

	resolveCounter := func(ctx context.Context) *TurnCounter {
		if c := TurnCounterFromContext(ctx); c != nil {
			return c
		}
		sessionID := protocol.TaskIDFrom(ctx)
		turnID := protocol.TurnIDFrom(ctx)
		if sessionID != "" && turnID != "" {
			key := sessionID + ":" + turnID
			scopedMu.Lock()
			defer scopedMu.Unlock()
			if c, ok := scopedCounters[key]; ok {
				return c
			}
			if len(scopedCounters) > 1024 {
				scopedCounters = make(map[string]*TurnCounter)
			}
			c := &TurnCounter{}
			scopedCounters[key] = c
			return c
		}
		return fallbackCounter
	}

	return func(ctx context.Context, input []byte) ([]byte, error) {
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

		// 回合内计数：优先 ctx 注入、其次 session+turn 隔离键、最后独立 fallback（R2-11）
		counter := resolveCounter(ctx)
		n := counter.Inc()
		if maxPerTurn > 0 && int(n) > maxPerTurn {
			return nil, apperr.New(apperr.CodeForbidden,
				"secret_get: 本回合已达凭据读取上限（secret_get.max_per_turn），防止批量泄露")
		}

		value, taintLevel, err := reader.GetSecret(ctx, args.Name)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "secret_get: read secret", err)
		}
		if taintLevel < types.TaintHigh {
			taintLevel = types.TaintHigh
		}
		_ = taintLevel // 凭据值标记 TaintHigh，由上层 tool 注册表通过 RegisterWithTaint 传播（R2-11）

		result := map[string]any{
			"name":  args.Name,
			"value": value,
		}
		return json.Marshal(result)
	}
}
