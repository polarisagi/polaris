package provider

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ProviderHandler LLM Provider 管理域依赖。
type ProviderHandler struct {
	ProviderRepo protocol.ProviderRepository
	ExtRepo      protocol.ExtensionRepository
	Registry     ProviderRegistry
	HTTPClient   *http.Client
	TBR          *metrics.TokenBurnRate
	DB           protocol.SQLQuerier // loaders and seeds might need DB directly
}

func NewProviderHandler(
	providerRepo protocol.ProviderRepository,
	extRepo protocol.ExtensionRepository,
	registry ProviderRegistry,
	httpClient *http.Client,
	tbr *metrics.TokenBurnRate,
	db protocol.SQLQuerier,
) *ProviderHandler {
	return &ProviderHandler{
		ProviderRepo: providerRepo,
		ExtRepo:      extRepo,
		Registry:     registry,
		HTTPClient:   httpClient,
		TBR:          tbr,
		DB:           db,
	}
}
