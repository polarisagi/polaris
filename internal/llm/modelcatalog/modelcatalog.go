// Package modelcatalog 提供 LLM 厂商实时模型目录探测、定时刷新与聚合能力（ADR-0115 WP3）。
package modelcatalog

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DiscoveredModel 对应 050_provider_discovered_models 表的一条记录。
type DiscoveredModel struct {
	ProviderID         string    `json:"provider_id"`
	ModelID            string    `json:"model_id"`
	DisplayName        string    `json:"display_name"`
	ContextWindow      int       `json:"context_window"`
	MaxOutput          int       `json:"max_output"`
	SupportsThinking   *bool     `json:"supports_thinking"`
	SupportsVision     *bool     `json:"supports_vision"`
	SupportsTools      *bool     `json:"supports_tools"`
	InputPricePerMTok  *float64  `json:"input_price_per_mtok"`
	OutputPricePerMTok *float64  `json:"output_price_per_mtok"`
	RawJSON            string    `json:"raw_json"`
	DiscoveredAt       time.Time `json:"discovered_at"`
	LastSeenAt         time.Time `json:"last_seen_at"`
}

// ProviderInfo 探测所需的厂商配置。
type ProviderInfo struct {
	ID        string
	Name      string
	Type      string
	BaseURL   string
	APIKey    string
	ProjectID string
	Location  string
	Enabled   bool
}

// CatalogManager 管理各启用厂商的模型探测与发现记录存储。
type CatalogManager struct {
	db         protocol.SQLQuerier
	vault      *credential.Vault
	httpClient *http.Client
}

// NewCatalogManager 构造模型目录管理器。
func NewCatalogManager(db protocol.SQLQuerier, vault *credential.Vault, httpClient *http.Client) *CatalogManager {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &CatalogManager{
		db:         db,
		vault:      vault,
		httpClient: httpClient,
	}
}

// RefreshProvider 针对指定厂商执行实时模型探测并将发现的模型写入数据库。
func (m *CatalogManager) RefreshProvider(ctx context.Context, providerID string) ([]DiscoveredModel, error) {
	prov, err := m.getProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !prov.Enabled {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("provider %s is disabled", providerID))
	}

	models, err := ProbeProviderModels(ctx, m.httpClient, *prov)
	if err != nil {
		return nil, err
	}

	if err := m.UpsertDiscoveredModels(ctx, models); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "modelcatalog: refreshed provider models", "provider_id", providerID, "discovered_count", len(models))
	return models, nil
}

// RefreshAll 遍历所有启用的厂商执行实时模型探测。
func (m *CatalogManager) RefreshAll(ctx context.Context) (map[string]int, error) {
	provs, err := m.listEnabledProviders(ctx)
	if err != nil {
		return nil, err
	}

	res := make(map[string]int)
	for _, p := range provs {
		models, err := ProbeProviderModels(ctx, m.httpClient, p)
		if err != nil {
			slog.WarnContext(ctx, "modelcatalog: probe provider models failed", "provider_id", p.ID, "err", err)
			continue
		}
		if err := m.UpsertDiscoveredModels(ctx, models); err != nil {
			slog.WarnContext(ctx, "modelcatalog: upsert discovered models failed", "provider_id", p.ID, "err", err)
			continue
		}
		res[p.ID] = len(models)
	}
	return res, nil
}

// UpsertDiscoveredModels 将发现的模型写入 provider_discovered_models 表。
func (m *CatalogManager) UpsertDiscoveredModels(ctx context.Context, models []DiscoveredModel) error {
	if len(models) == 0 {
		return nil
	}

	query := `
INSERT INTO provider_discovered_models (
    provider_id, model_id, display_name, context_window, max_output,
    supports_thinking, supports_vision, supports_tools,
    input_price_per_mtok, output_price_per_mtok, raw_json,
    discovered_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now'))
ON CONFLICT(provider_id, model_id) DO UPDATE SET
    display_name = COALESCE(excluded.display_name, provider_discovered_models.display_name),
    context_window = CASE WHEN excluded.context_window > 0 THEN excluded.context_window ELSE provider_discovered_models.context_window END,
    max_output = CASE WHEN excluded.max_output > 0 THEN excluded.max_output ELSE provider_discovered_models.max_output END,
    supports_thinking = COALESCE(excluded.supports_thinking, provider_discovered_models.supports_thinking),
    supports_vision = COALESCE(excluded.supports_vision, provider_discovered_models.supports_vision),
    supports_tools = COALESCE(excluded.supports_tools, provider_discovered_models.supports_tools),
    input_price_per_mtok = COALESCE(excluded.input_price_per_mtok, provider_discovered_models.input_price_per_mtok),
    output_price_per_mtok = COALESCE(excluded.output_price_per_mtok, provider_discovered_models.output_price_per_mtok),
    raw_json = excluded.raw_json,
    last_seen_at = strftime('%Y-%m-%dT%H:%M:%SZ','now');`

	for _, mod := range models {
		var thinkVal, visVal, toolVal any
		if mod.SupportsThinking != nil {
			if *mod.SupportsThinking {
				thinkVal = 1
			} else {
				thinkVal = 0
			}
		}
		if mod.SupportsVision != nil {
			if *mod.SupportsVision {
				visVal = 1
			} else {
				visVal = 0
			}
		}
		if mod.SupportsTools != nil {
			if *mod.SupportsTools {
				toolVal = 1
			} else {
				toolVal = 0
			}
		}

		var inPrice, outPrice any
		if mod.InputPricePerMTok != nil {
			inPrice = *mod.InputPricePerMTok
		}
		if mod.OutputPricePerMTok != nil {
			outPrice = *mod.OutputPricePerMTok
		}

		var disp any
		if mod.DisplayName != "" {
			disp = mod.DisplayName
		}

		_, err := m.db.ExecContext(ctx, query,
			mod.ProviderID, mod.ModelID, disp, mod.ContextWindow, mod.MaxOutput,
			thinkVal, visVal, toolVal, inPrice, outPrice, mod.RawJSON,
		)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("modelcatalog: upsert %s/%s", mod.ProviderID, mod.ModelID), err)
		}
	}
	return nil
}

// ListDiscoveredModels 列出数据库中记录的实时发现模型。
func (m *CatalogManager) ListDiscoveredModels(ctx context.Context, providerID string) ([]DiscoveredModel, error) {
	var query string
	var args []any
	if providerID != "" {
		query = `SELECT provider_id, model_id, COALESCE(display_name, ''), context_window, max_output,
                        supports_thinking, supports_vision, supports_tools,
                        input_price_per_mtok, output_price_per_mtok, COALESCE(raw_json, ''),
                        discovered_at, last_seen_at
                   FROM provider_discovered_models
                  WHERE provider_id = ?
                  ORDER BY model_id`
		args = append(args, providerID)
	} else {
		query = `SELECT provider_id, model_id, COALESCE(display_name, ''), context_window, max_output,
                        supports_thinking, supports_vision, supports_tools,
                        input_price_per_mtok, output_price_per_mtok, COALESCE(raw_json, ''),
                        discovered_at, last_seen_at
                   FROM provider_discovered_models
                  ORDER BY provider_id, model_id`
	}

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "modelcatalog: list discovered models", err)
	}
	defer rows.Close()

	var out []DiscoveredModel
	for rows.Next() {
		var mod DiscoveredModel
		var thinkVal, visVal, toolVal sql.NullInt64
		var inPrice, outPrice sql.NullFloat64
		var discAt, seenAt string

		if err := rows.Scan(
			&mod.ProviderID, &mod.ModelID, &mod.DisplayName, &mod.ContextWindow, &mod.MaxOutput,
			&thinkVal, &visVal, &toolVal, &inPrice, &outPrice, &mod.RawJSON,
			&discAt, &seenAt,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "modelcatalog: scan discovered model", err)
		}

		if thinkVal.Valid {
			b := thinkVal.Int64 == 1
			mod.SupportsThinking = &b
		}
		if visVal.Valid {
			b := visVal.Int64 == 1
			mod.SupportsVision = &b
		}
		if toolVal.Valid {
			b := toolVal.Int64 == 1
			mod.SupportsTools = &b
		}
		if inPrice.Valid {
			v := inPrice.Float64
			mod.InputPricePerMTok = &v
		}
		if outPrice.Valid {
			v := outPrice.Float64
			mod.OutputPricePerMTok = &v
		}

		mod.DiscoveredAt, _ = time.Parse(time.RFC3339, discAt)
		mod.LastSeenAt, _ = time.Parse(time.RFC3339, seenAt)
		out = append(out, mod)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "modelcatalog: rows iteration", err)
	}
	return out, nil
}

func (m *CatalogManager) getProvider(ctx context.Context, id string) (*ProviderInfo, error) {
	row := m.db.QueryRowContext(ctx, `
		SELECT id, name, type, COALESCE(base_url, ''), COALESCE(api_key, ''), COALESCE(project_id, ''), COALESCE(location, ''), enabled
		  FROM providers WHERE id = ?`, id)

	var p ProviderInfo
	var enabled int
	if err := row.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKey, &p.ProjectID, &p.Location, &enabled); err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, fmt.Sprintf("provider %s not found", id), err)
	}
	p.Enabled = enabled == 1

	if m.vault != nil && p.APIKey != "" {
		if dec, err := m.vault.Decrypt(p.APIKey); err == nil {
			p.APIKey = dec
		}
	}
	return &p, nil
}

func (m *CatalogManager) listEnabledProviders(ctx context.Context) ([]ProviderInfo, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, name, type, COALESCE(base_url, ''), COALESCE(api_key, ''), COALESCE(project_id, ''), COALESCE(location, ''), enabled
		  FROM providers WHERE enabled = 1`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "modelcatalog: listEnabledProviders", err)
	}
	defer rows.Close()

	var out []ProviderInfo
	for rows.Next() {
		var p ProviderInfo
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKey, &p.ProjectID, &p.Location, &enabled); err != nil {
			continue
		}
		p.Enabled = enabled == 1
		if m.vault != nil && p.APIKey != "" {
			if dec, err := m.vault.Decrypt(p.APIKey); err == nil {
				p.APIKey = dec
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, apperr.Wrap(apperr.CodeInternal, "iterate providers failed", err)
	}
	return out, nil
}
