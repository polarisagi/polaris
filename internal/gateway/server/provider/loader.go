package provider

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"fmt"
	"net/http"

	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
)

// LoadProvidersFromDB 从 providers + provider_models 两表 JOIN，
// 每个启用的 (provider, model) 组合注册一个带角色的 Adapter 到 ProviderRegistry。
func LoadProvidersFromDB(ctx context.Context, db protocol.SQLQuerier, reg ProviderRegistry, httpClient *http.Client, tbr *metrics.TokenBurnRate) error {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.name, p.type, COALESCE(p.base_url, ''), COALESCE(p.api_key, ''), COALESCE(p.project_id, ''), COALESCE(p.location, ''),
		       m.id, COALESCE(m.name, ''), m.model_id, m.role
		  FROM providers p
		  JOIN provider_models m ON m.provider_id = p.id
		 WHERE p.enabled=1 AND m.enabled=1
		 ORDER BY p.created_at, m.created_at`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "LoadProvidersFromDB", err)
	}
	defer rows.Close()

	reg.UnregisterAll()

	for rows.Next() {
		var pID, pName, typ, baseURL, apiKeyStr, projectID, location string
		var mID, mName, modelID, role string
		if err := rows.Scan(&pID, &pName, &typ, &baseURL, &apiKeyStr, &projectID, &location,
			&mID, &mName, &modelID, &role); err != nil {
			continue
		}

		// 将 key 转为 []byte 一次，避免多次分配；SQL 行扫描后 apiKeyStr 字符串本身
		// 仍在堆上，但 keyBytes 在 for 循环结束后可被 GC 回收，生命周期更短。
		// credFn 每次返回副本（Adapter 使用后应由其调用方及时 GC）。
		keyBytes := []byte(apiKeyStr)
		credFn := func() []byte {
			cp := make([]byte, len(keyBytes))
			copy(cp, keyBytes)
			return cp
		}

		suffix := mID
		if len(mID) > 8 {
			suffix = mID[:8]
		}
		// 注册名使用 model 记录 ID 前缀，保证同厂商多模型不冲突
		name := fmt.Sprintf("%s/%s", typ, suffix)

		displayName := mName
		if displayName == "" {
			displayName = modelID
		}
		displayName = fmt.Sprintf("[%s] %s", pName, displayName)

		switch typ {
		case "openai_compat":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewOpenAIAdapter(baseURL, modelID, credFn, httpClient, tbr))
		case "anthropic":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewAnthropicAdapter(modelID, credFn, httpClient, tbr))
		case "google_agent_platform":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewGoogleAgentPlatformAdapter(modelID, projectID, location, credFn, httpClient, tbr))
		case "ollama":
			if baseURL == "" {
				baseURL = "http://localhost:11434"
			}
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewOpenAIAdapter(baseURL+"/v1", modelID, credFn, httpClient, tbr))
		}
		// 提示 GC：keyBytes 在 credFn 捕获后不再需要单独持有
		// 注意：credFn 的闭包已持有 keyBytes 引用，此行不改变内存安全性，
		// 仅文档意图：keyBytes 本变量在此之后不再被直接使用。
		_ = keyBytes
	}
	return rows.Err()
}
