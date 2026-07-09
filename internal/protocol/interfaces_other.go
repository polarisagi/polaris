package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// HybridRetriever 是 BM25 + Dense Vector + Graph Traversal 三路融合检索的统一接口。
// M5 与 M10 共享底层 RRF 融合 + Rerank 引擎，检索范围和配置参数各自独立。
// 检索配置差异: M5 FinalTopK=10, RerankTopM=30; M10 FinalTopK=5, RerankTopM=50。
HybridRetriever interface {
	Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error)
}

type

// PolicyGate 是 Cedar 策略引擎的 Go 接口。
// 原则: deny-by-default + forbid 无条件优先于 permit。
// FFI 调用失败 → deny（fail-closed）。Evaluate 超时 >10ms → deny + 计数器递增。
// 连续 10 次 Evaluate 失败 → KillSwitch Stage 1 THROTTLE。
PolicyGate interface {
	IsAuthorized(ctx context.Context, principal, action, resource string, context map[string]any) (bool, error)
	Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error)
}

type

// PreferencesRepo 提供对系统偏好的访问。
PreferencesRepo interface {
	GetPermissionMode(ctx context.Context) (types.PermissionMode, error)
	SetPermissionMode(ctx context.Context, mode types.PermissionMode) error
}

type

// HITL 是人工审批网关。
HITL interface {
	Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)
	Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error
	Pending(ctx context.Context) ([]types.HITLPrompt, error)
}
