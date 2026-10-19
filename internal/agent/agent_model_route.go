package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type modelRouteDecision struct {
	Ref      string
	Pool     string
	Thinking string
	Source   string
	Fallback string
}

// resolveModelRoute 解析当前 LLM 调用的目标模型引用、角色池与思考档。
// 优先级仲裁：
// 用户手选 (UserLockedModel) > 角色指定 (AgentProfile.Model) > 循环请求 (NextRoundModelRef) > 阶段默认池
func (a *Agent) resolveModelRoute(ctx context.Context, defaultPool, defaultThinking string) (ref string, pool string, thinking string, err error) {
	a.sCtx.Mu.Lock()
	userLocked := a.sCtx.UserLockedModel
	var profileModel, profileThinking string
	if a.sCtx.AgentProfile != nil {
		profileModel = a.sCtx.AgentProfile.Model
		profileThinking = a.sCtx.AgentProfile.Thinking
	}
	nextRef := a.sCtx.NextRoundModelRef
	nextThinking := a.sCtx.NextRoundThinking
	// 消费 NextRound 字段（仅下一轮生效一次）
	a.sCtx.NextRoundModelRef = ""
	a.sCtx.NextRoundThinking = ""
	a.sCtx.Mu.Unlock()

	dec := modelRouteDecision{
		Pool:     defaultPool,
		Thinking: defaultThinking,
		Source:   "default",
		Fallback: "none",
	}

	switch {
	case userLocked != "":
		dec.Source = "user_lock"
		dec.Ref = userLocked
	case profileModel != "":
		dec.Source = "agent_profile"
		dec.Ref = profileModel
		if profileThinking != "" {
			dec.Thinking = profileThinking
		}
	case nextRef != "":
		dec.Source = "next_round"
		dec.Ref = nextRef
		if nextThinking != "" {
			dec.Thinking = nextThinking
		}
	default:
		dec.Source = "default"
	}

	// 若引用的值为已知 ModelPool 名称（如 general, reasoning, default, budget），直接作为 Pool
	if dec.Ref != "" && types.IsKnownPool(dec.Ref) {
		dec.Pool = dec.Ref
		dec.Ref = ""
	}

	// 如果有具体引用模型，进行隐私与成本校验
	if err := a.applyModelValidation(ctx, &dec); err != nil {
		return "", "", "", err
	}

	a.sCtx.Mu.Lock()
	if dec.Ref != "" {
		a.sCtx.CurrentModelRef = dec.Ref
	} else {
		a.sCtx.CurrentModelRef = dec.Pool
	}
	a.sCtx.Mu.Unlock()

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(
			attribute.String("model.route.source", dec.Source),
			attribute.String("model.route.ref", dec.Ref),
			attribute.String("model.route.pool", dec.Pool),
			attribute.String("model.route.thinking", dec.Thinking),
			attribute.String("model.route.fallback", dec.Fallback),
		)
	}

	return dec.Ref, dec.Pool, dec.Thinking, nil
}

func (a *Agent) applyModelValidation(ctx context.Context, dec *modelRouteDecision) error {
	if dec.Ref == "" {
		return nil
	}
	vErr := a.validateModelSelection(ctx, dec.Ref)
	if vErr == nil {
		return nil
	}
	if strings.Contains(vErr.Error(), "local_only_mode") {
		dec.Fallback = "local_only"
		metrics.RecordAgentModelRoute(ctx, dec.Source, dec.Fallback)
		slog.WarnContext(ctx, "agent: non-local model rejected by local_only_mode",
			"ref", dec.Ref, "source", dec.Source)
		return vErr
	}
	if strings.Contains(vErr.Error(), "max_output_price_per_mtok") {
		dec.Fallback = "cost_limit"
	} else {
		dec.Fallback = "error"
	}
	metrics.RecordAgentModelRoute(ctx, dec.Source, dec.Fallback)
	return vErr
}

// GetModelStatus 返回当前模型及用户手选锁定状态。
func (a *Agent) GetModelStatus() (currentModel string, userLocked string) {
	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	return a.sCtx.CurrentModelRef, a.sCtx.UserLockedModel
}

// SwitchModel 请求在下一轮切换模型或思考档（ADR-0115 WP3）。
func (a *Agent) SwitchModel(ref, pool, thinking string) (string, error) {
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()

	if a.sCtx.UserLockedModel != "" {
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("当前模型已被用户在界面锁定为 %s，无法自选切换", a.sCtx.UserLockedModel))
	}

	target := ref
	if target == "" {
		target = pool
	}
	if target == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "必须指定 ref 或 pool")
	}

	if err := a.validateModelSelection(context.Background(), target); err != nil {
		return "", err
	}

	a.sCtx.NextRoundModelRef = target
	a.sCtx.NextRoundThinking = thinking
	return fmt.Sprintf("已成功请求下一轮切换模型为 %s（thinking: %s）。注意：切换模型会导致前缀缓存失效（T4 成本），请谨慎使用。", target, thinking), nil
}

func (a *Agent) validateModelSelection(ctx context.Context, ref string) error {
	cfg := config.Get()
	localOnly := false
	if cfg != nil {
		localOnly = cfg.Security.LocalOnlyMode
	}
	maxPrice := config.CurrentThresholds().M1Router.ModelSelectMaxOutputPricePerMtok

	if !localOnly && maxPrice <= 0 {
		return nil
	}

	providerID, modelID := parseRef(ref)
	if a.extQuerier == nil {
		if localOnly && !isLocalRef(providerID) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("local_only_mode is enabled: model ref %q is not local", ref))
		}
		return nil
	}

	return a.validateModelFromDB(ctx, ref, providerID, modelID, localOnly, maxPrice)
}

func parseRef(ref string) (providerID, modelID string) {
	if strings.Contains(ref, "/") {
		parts := strings.SplitN(ref, "/", 2)
		return parts[0], parts[1]
	}
	return ref, ""
}

func (a *Agent) validateModelFromDB(ctx context.Context, ref, providerID, modelID string, localOnly bool, maxPrice float64) error {
	var pType string
	var outputPrice float64
	var hasDiscovered int

	row := a.extQuerier.QueryRowContext(ctx, `
		SELECT p.type, COALESCE(d.output_price_per_mtok, 0), CASE WHEN d.model_id IS NOT NULL THEN 1 ELSE 0 END
		  FROM providers p
		  LEFT JOIN provider_discovered_models d ON d.provider_id = p.id AND d.model_id = ?
		 WHERE p.id = ? OR p.type = ? OR p.name = ?
		 LIMIT 1`, modelID, providerID, providerID, providerID)

	if err := row.Scan(&pType, &outputPrice, &hasDiscovered); err != nil {
		if localOnly && !isLocalRef(providerID) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("local_only_mode is enabled: unknown or external model %q", ref))
		}
		return nil
	}

	isLocal := pType == "ollama" || pType == "llama_local"
	if localOnly && !isLocal {
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("local_only_mode is enabled: provider type %q for model %q is not local", pType, ref))
	}

	if maxPrice > 0 && hasDiscovered == 1 && outputPrice > maxPrice {
		return apperr.New(apperr.CodeResourceExhausted, fmt.Sprintf("model %q price %f exceeds max_output_price_per_mtok threshold %f", ref, outputPrice, maxPrice))
	}

	return nil
}

func isLocalRef(ref string) bool {
	lower := strings.ToLower(ref)
	return strings.HasPrefix(lower, "ollama") || strings.HasPrefix(lower, "llama") || strings.Contains(lower, "local")
}
