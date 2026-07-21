package provider

import (
	"github.com/polarisagi/polaris/internal/llm/modelregistry"
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ProviderHandler LLM Provider 管理域依赖。
type ProviderHandler struct {
	ProviderRepo    protocol.ProviderRepository
	ExtRepo         protocol.ExtensionRepository
	Registry        ProviderRegistry
	HTTPClient      *http.Client
	TBR             *metrics.TokenBurnRate
	ReloadProviders func()
	DB              protocol.SQLQuerier // loaders and seeds might need DB directly
	// ModelRegistry P3-2 ModelVersionRegistry 运营触发入口（2026-07-21 deadcode 审查补齐）：
	// "厂商发布新模型版本"本质是运营人工判断，不可自动探测，故只提供 HTTP 触发点，
	// 由运营在确认新版本可用后调用；nil 时 HandleModelUpgrade/HandleModelDeprecate
	// 返回 503（构造函数不做 fail-closed，同 NewProviderHandler 既有原则）。
	ModelRegistry *modelregistry.Registry
}

type Dependencies struct {
	ProviderRepo  protocol.ProviderRepository
	ExtRepo       protocol.ExtensionRepository
	Registry      ProviderRegistry
	HTTPClient    *http.Client
	TBR           *metrics.TokenBurnRate
	DB            protocol.SQLQuerier
	ModelRegistry *modelregistry.Registry
}

// NewProviderHandler 故意不做构造函数级 fail-closed nil 强制校验，结论与理由见
// chat.NewChatHandler 文档注释 + local_playground/reports/
// phase4-hard-dep-and-deadcode-followup-20260708.md（HTTP 路径有 PanicRecovery
// 中间件兜底，真正的进程级崩溃风险在后台 goroutine，非构造函数）。
func NewProviderHandler(deps Dependencies) *ProviderHandler {
	return &ProviderHandler{
		ProviderRepo:  deps.ProviderRepo,
		ExtRepo:       deps.ExtRepo,
		Registry:      deps.Registry,
		HTTPClient:    deps.HTTPClient,
		TBR:           deps.TBR,
		DB:            deps.DB,
		ModelRegistry: deps.ModelRegistry,
	}
}
