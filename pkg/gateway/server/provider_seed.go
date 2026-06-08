package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"os"
	"time"
)

// envProviderSeed 描述一个由环境变量驱动的 provider 种子。
type envProviderSeed struct {
	id      string // 固定 ID，防重复（prov_env_openai 等）
	name    string // UI 显示名，含 [env] 标注
	typ     string // openai_compat | anthropic
	baseURL string
	envKey  string // 读取 API Key 的环境变量名
	models  []envModelSeed
}

type envModelSeed struct {
	modelID string
	name    string
	role    string // general（不自动抢占 default 角色，让用户手动设置）
}

// envSeeds 定义支持自动导入的 env var → provider 映射表。
var envSeeds = []envProviderSeed{
	{
		id:      "prov_env_openai",
		name:    "OpenAI [env]",
		typ:     "openai_compat",
		baseURL: "https://api.openai.com",
		envKey:  "OPENAI_API_KEY",
		models: []envModelSeed{
			{modelID: "gpt-4o", name: "GPT-4o", role: "general"},
		},
	},
	{
		id:      "prov_env_anthropic",
		name:    "Anthropic [env]",
		typ:     "anthropic",
		baseURL: "",
		envKey:  "ANTHROPIC_API_KEY",
		models: []envModelSeed{
			{modelID: "claude-3-7-sonnet-20250219", name: "Claude 3.7 Sonnet", role: "general"},
		},
	},
	{
		id:      "prov_env_deepseek",
		name:    "DeepSeek [env]",
		typ:     "openai_compat",
		baseURL: "https://api.deepseek.com",
		envKey:  "DEEPSEEK_API_KEY",
		models: []envModelSeed{
			{modelID: "deepseek-chat", name: "DeepSeek Chat", role: "general"},
		},
	},
}

// SeedProvidersFromEnv 在启动时检测 API Key 环境变量，将发现的凭据以 INSERT OR IGNORE 写入 DB。
//
// 规则：
//   - INSERT OR IGNORE：DB 中已存在同 ID 条目则跳过，不覆盖用户在 UI 里的修改。
//   - 写入的 provider 默认 enabled=1，用户可在 UI 中禁用/修改/删除。
//   - 角色固定为 general，不自动抢占 default/reasoning，由用户在模型配置页手动分配。
//   - 检测结果写入 slog，保证行为可观测。
//
// 调用时机：存储初始化完成之后、LoadProvidersFromDB 之前。
func SeedProvidersFromEnv(ctx context.Context, db *sql.DB) {
	now := time.Now().UTC().Format(time.RFC3339)

	for _, seed := range envSeeds {
		apiKey := os.Getenv(seed.envKey)
		if apiKey == "" {
			continue // 未设置该 env var，跳过
		}

		// 写入 providers 表（已存在则忽略，不覆盖）
		res, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO providers
			    (id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, '', '', '', 1, ?, ?)`,
			seed.id, seed.name, seed.typ, seed.baseURL, apiKey, now, now,
		)
		if err != nil {
			slog.Warn("polaris: SeedProvidersFromEnv: providers insert failed",
				"id", seed.id, "err", err)
			continue
		}

		inserted, _ := res.RowsAffected()
		if inserted > 0 {
			slog.Info("polaris: env provider seeded into DB",
				"id", seed.id, "name", seed.name, "type", seed.typ,
				"env_var", seed.envKey)
		} else {
			// 已存在，只更新 api_key（防止 key 轮换后旧 key 留在 DB）
			_, _ = db.ExecContext(ctx,
				`UPDATE providers SET api_key=?, updated_at=? WHERE id=?`,
				apiKey, now, seed.id,
			)
			slog.Info("polaris: env provider already in DB, api_key refreshed",
				"id", seed.id, "env_var", seed.envKey)
		}

		// 写入 provider_models 表（已存在则忽略）
		for _, m := range seed.models {
			modelRecID := modelEnvID(seed.id, m.modelID)
			_, err := db.ExecContext(ctx,
				`INSERT OR IGNORE INTO provider_models
				    (id, provider_id, model_id, name, role, enabled, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
				modelRecID, seed.id, m.modelID, m.name, m.role, now, now,
			)
			if err != nil {
				slog.Warn("polaris: SeedProvidersFromEnv: provider_models insert failed",
					"model_id", m.modelID, "err", err)
			}
		}
	}
}

// modelEnvID 生成稳定的 model 记录 ID（seed ID + 短 hash，避免与用户创建的 mdl_ 前缀冲突）。
func modelEnvID(providerID, modelID string) string {
	// 使用确定性 ID：providerID_modelID 的截断，保证 INSERT OR IGNORE 幂等
	raw := providerID + "_" + modelID
	if len(raw) > 32 {
		raw = raw[:32]
	}
	return "mdl_env_" + hex.EncodeToString([]byte(raw))[:12]
}
