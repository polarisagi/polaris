package provider

import (
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
}

type Dependencies struct {
	ProviderRepo protocol.ProviderRepository
	ExtRepo      protocol.ExtensionRepository
	Registry     ProviderRegistry
	HTTPClient   *http.Client
	TBR          *metrics.TokenBurnRate
	DB           protocol.SQLQuerier
}

// NewProviderHandler 故意不做构造函数级 fail-closed nil 强制校验，结论与理由见
// chat.NewChatHandler 文档注释 + local_playground/reports/
// phase4-hard-dep-and-deadcode-followup-20260708.md（HTTP 路径有 PanicRecovery
// 中间件兜底，真正的进程级崩溃风险在后台 goroutine，非构造函数）。
func NewProviderHandler(deps Dependencies) *ProviderHandler {
	return &ProviderHandler{
		ProviderRepo: deps.ProviderRepo,
		ExtRepo:      deps.ExtRepo,
		Registry:     deps.Registry,
		HTTPClient:   deps.HTTPClient,
		TBR:          deps.TBR,
		DB:           deps.DB,
	}
}
