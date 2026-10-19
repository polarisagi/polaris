package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type dbProviderFactory struct {
	db         protocol.SQLQuerier
	vault      *credential.Vault
	httpClient *http.Client
	tbr        *metrics.TokenBurnRate
}

// NewDBProviderFactory 创建一个由数据库凭据驱动的 Provider 工厂。
func NewDBProviderFactory(db protocol.SQLQuerier, vault *credential.Vault, httpClient *http.Client, tbr *metrics.TokenBurnRate) llm.ProviderFactory {
	return &dbProviderFactory{
		db:         db,
		vault:      vault,
		httpClient: httpClient,
		tbr:        tbr,
	}
}

// CreateProvider 按 providerID 和 modelID 从 DB 读取厂商凭据并构造 Adapter。
func (f *dbProviderFactory) CreateProvider(ctx context.Context, providerID, modelID string) (protocol.Provider, string, error) {
	if f.db == nil {
		return nil, "", apperr.New(apperr.CodeInternal, "db is nil in dbProviderFactory")
	}

	row := f.db.QueryRowContext(ctx, `
		SELECT name, type, COALESCE(base_url, ''), COALESCE(api_key, ''), COALESCE(project_id, ''), COALESCE(location, '')
		  FROM providers
		 WHERE id = ? AND enabled = 1`, providerID)

	var pName, typ, baseURL, apiKeyStr, projectID, location string
	if err := row.Scan(&pName, &typ, &baseURL, &apiKeyStr, &projectID, &location); err != nil {
		return nil, "", apperr.Wrap(apperr.CodeNotFound, "provider not found or disabled", err)
	}

	if cfg := config.Get(); cfg != nil && cfg.Security.LocalOnlyMode {
		isLocal := typ == "ollama" || typ == "llama_local"
		if !isLocal {
			return nil, "", apperr.New(apperr.CodeForbidden, fmt.Sprintf("local_only_mode is enabled: provider type %q for model %q is not local", typ, modelID))
		}
	}

	if f.vault != nil && apiKeyStr != "" {
		if dec, err := f.vault.Decrypt(apiKeyStr); err == nil {
			apiKeyStr = dec
		} else {
			slog.Warn("dbProviderFactory: failed to decrypt api key", "provider_id", providerID, "err", err)
		}
	}

	credPool := llm.NewCredentialPool(splitAPIKeys(apiKeyStr), llm.StrategyRoundRobin)
	p := buildProviderAdapter(typ, baseURL, modelID, projectID, location, credPool, f.httpClient, f.tbr)
	if p == nil {
		return nil, "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("unsupported provider type %q or adapter build failed", typ))
	}

	displayName := fmt.Sprintf("[%s] %s", pName, modelID)
	return p, displayName, nil
}
