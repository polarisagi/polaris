package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/polarisagi/polaris/pkg/substrate/inference"
)

// LoadProvidersFromDB 从 providers + provider_models 两表 JOIN，
// 每个启用的 (provider, model) 组合注册一个带角色的 Adapter 到 ProviderRegistry。
func LoadProvidersFromDB(ctx context.Context, db *sql.DB, reg *inference.ProviderRegistry, httpClient *http.Client) error {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.name, p.type, p.base_url, p.api_key, p.project_id, p.location,
		       m.id, m.name, m.model_id, m.role
		  FROM providers p
		  JOIN provider_models m ON m.provider_id = p.id
		 WHERE p.enabled=1 AND m.enabled=1
		 ORDER BY p.created_at, m.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()

	reg.UnregisterAll()

	for rows.Next() {
		var pID, pName, typ, baseURL, apiKey, projectID, location string
		var mID, mName, modelID, role string
		if err := rows.Scan(&pID, &pName, &typ, &baseURL, &apiKey, &projectID, &location,
			&mID, &mName, &modelID, &role); err != nil {
			continue
		}

		keyCopy := apiKey
		credFn := func() string { return keyCopy }
		// 注册名使用 model 记录 ID 前缀，保证同厂商多模型不冲突
		name := fmt.Sprintf("%s/%s", typ, mID[:8])

		displayName := mName
		if displayName == "" {
			displayName = modelID
		}
		displayName = fmt.Sprintf("[%s] %s", pName, displayName)

		switch typ {
		case "openai_compat":
			reg.RegisterWithRole(name, displayName, role, inference.NewOpenAIAdapter(baseURL, modelID, credFn, httpClient))
		case "anthropic":
			reg.RegisterWithRole(name, displayName, role, inference.NewAnthropicAdapter(modelID, credFn, httpClient))
		case "google_agent_platform":
			reg.RegisterWithRole(name, displayName, role, inference.NewGoogleAgentPlatformAdapter(modelID, projectID, location, credFn, httpClient))
		case "ollama":
			if baseURL == "" {
				baseURL = "http://localhost:11434"
			}
			reg.RegisterWithRole(name, displayName, role, inference.NewOpenAIAdapter(baseURL+"/v1", modelID, credFn, httpClient))
		}
	}
	return rows.Err()
}
