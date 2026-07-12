package main

import (
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/llm"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"
)

// LoadProvidersFromDB 从 providers + provider_models 两表 JOIN，
// 每个启用的 (provider, model) 组合注册一个带角色的 Adapter 到 ProviderRegistry。
func LoadProvidersFromDB(ctx context.Context, db protocol.SQLQuerier, vault *credential.Vault, reg provider.ProviderRegistry, httpClient *http.Client, tbr *metrics.TokenBurnRate) error {
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

		if vault != nil {
			if dec, err := vault.Decrypt(apiKeyStr); err == nil {
				apiKeyStr = dec
			} else {
				slog.Warn("polaris: LoadProvidersFromDB failed to decrypt api key", "provider_id", pID, "err", err)
			}
		}

		// 2026-07-12 P1 修复：api_key 列支持按行存放多个 Key（每行一个），
		// 构造 CredentialPool 实现同一 Provider 记录下的多 Key 轮换 + 失败自动冷却
		// （401/402/429 等触发 PooledCredential.RecordResult 冷却，不再是"单 Key
		// 失效即整个 Provider 不可用"）。单 Key 场景行为与旧版 credPool 完全等价。
		credPool := llm.NewCredentialPool(splitAPIKeys(apiKeyStr), llm.StrategyRoundRobin)

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
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewOpenAIAdapter(baseURL, modelID, credPool, httpClient, tbr))
		case "anthropic":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewAnthropicAdapter(modelID, credPool, httpClient, tbr))
		case "deepseek":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewDeepSeekAdapter(credPool, httpClient, modelID, tbr))
		case "google_agent_platform":
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewGoogleAgentPlatformAdapter(modelID, projectID, location, credPool, httpClient, tbr))
		case "ollama":
			if baseURL == "" {
				baseURL = "http://localhost:11434"
			}
			reg.RegisterWithRole(name, displayName, role, llmadapter.NewOpenAIAdapter(baseURL+"/v1", modelID, credPool, httpClient, tbr))
		}
	}
	if err := rows.Err(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rows error", err)
	}
	return nil
}

// splitAPIKeys 将 providers.api_key 列解析为凭证列表：按行分隔，去除首尾空白，
// 丢弃空行。单 key（无换行）场景与旧版行为完全一致；多行则启用 CredentialPool
// 轮换 + 失败冷却（P1 2026-07-12）。DB 列本身不变更 schema，纯文本约定即可支持
// 多 Key，避免上线前就引入新表/新增管理 API 的额外风险。
func splitAPIKeys(raw string) []string {
	lines := strings.Split(raw, "\n")
	keys := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			keys = append(keys, l)
		}
	}
	return keys
}
