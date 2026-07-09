package provider

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
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

// builtinEnvSeeds 定义支持自动导入的 env var → provider 映射表。
func builtinEnvSeeds() []envProviderSeed {
	return []envProviderSeed{}
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
func SeedProvidersFromEnv(ctx context.Context, repo protocol.ProviderRepository) {
	now := time.Now().UTC().Format(time.RFC3339)

	for _, seed := range builtinEnvSeeds() {
		apiKey := os.Getenv(seed.envKey)
		if apiKey == "" {
			continue // 未设置该 env var，跳过
		}

		// 写入 providers 表（已存在则忽略，不覆盖）
		inserted, err := repo.SeedFromEnv(ctx, types.ProviderRow{
			ID:        seed.id,
			Name:      seed.name,
			Type:      seed.typ,
			BaseURL:   seed.baseURL,
			APIKey:    apiKey,
			Enabled:   true,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			slog.Warn("polaris: SeedProvidersFromEnv: providers insert failed",
				"id", seed.id, "err", err)
			continue
		}

		if inserted {
			slog.Info("polaris: env provider seeded into DB",
				"id", seed.id, "name", seed.name, "type", seed.typ,
				"env_var", seed.envKey)
		} else {
			// 已存在，只更新 api_key（防止 key 轮换后旧 key 留在 DB）
			_ = repo.UpdateProviderAPIKey(ctx, seed.id, apiKey, now)
			slog.Info("polaris: env provider already in DB, api_key refreshed",
				"id", seed.id, "env_var", seed.envKey)
		}

		// 写入 provider_models 表（已存在则忽略）
		for _, m := range seed.models {
			modelRecID := modelEnvID(seed.id, m.modelID)
			err := repo.SeedModelFromEnv(ctx, types.ProviderModelRow{
				ID:         modelRecID,
				ProviderID: seed.id,
				ModelID:    m.modelID,
				Name:       m.name,
				Role:       m.role,
				Enabled:    true,
				CreatedAt:  now,
				UpdatedAt:  now,
			})
			if err != nil {
				slog.Warn("polaris: SeedProvidersFromEnv: provider_models insert failed",
					"id", modelRecID, "err", err)
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
