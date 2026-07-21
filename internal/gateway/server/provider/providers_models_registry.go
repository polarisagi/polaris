package provider

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
)

// ============================================================================
// ModelVersionRegistry 运营触发入口（2026-07-21 deadcode 审查补齐，M01 §9）。
//
// "厂商发布了新模型版本 / 决定废弃某个旧模型" 是运营侧的人工判断，无法从既有
// DB 数据自动探测（不同于 DriftDetector 之类可由指标驱动的自动检测），故只提供
// HTTP 触发点：运营在确认新版本可用/需要废弃旧版本后显式调用，registry 内部
// DecideMigration 三档策略负责后续判断是否需要人工确认。
//
// tester 恒传 nil：SkillCompatTester 目前无生产实现（见 registry.go 文档），
// OnModelUpgrade 会据此跳过重测、仅刷新 UpdatedAt + 记录迁移策略日志，等到
// internal/eval/harness 或 internal/extension/skill 提供具体实现后再补上。
// ============================================================================

// HandleModelUpgrade 触发 ModelVersionRegistry.OnModelUpgrade：运营确认某个
// provider/model 已升级可用后调用，用于刷新兼容性评分与迁移策略档位。
// Body: {"skill_names": ["..."]}（可选，留空则只刷新元数据不做兼容测试）。
func (h *ProviderHandler) HandleModelUpgrade(w http.ResponseWriter, r *http.Request) {
	if h.ModelRegistry == nil {
		http.Error(w, `{"error":"model registry not available"}`, http.StatusServiceUnavailable)
		return
	}
	providerID := r.PathValue("providerID")
	modelID := r.PathValue("modelID")

	var req struct {
		SkillNames []string `json:"skill_names"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // 空 body 合法：只刷新元数据
	}

	if err := h.ModelRegistry.OnModelUpgrade(r.Context(), providerID, modelID, req.SkillNames, nil); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	entry, err := h.ModelRegistry.Get(r.Context(), providerID, modelID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "entry": entry})
}

// HandleModelDeprecate 触发 ModelVersionRegistry.DeprecateModel：运营决定
// 废弃某个 provider/model 并指定继任模型；若该模型具备 embedding 能力，
// 内部会唤醒 ReindexTrigger（见 registry.go DeprecateModel 文档）。
// Body: {"successor_model_id": "..."}。
func (h *ProviderHandler) HandleModelDeprecate(w http.ResponseWriter, r *http.Request) {
	if h.ModelRegistry == nil {
		http.Error(w, `{"error":"model registry not available"}`, http.StatusServiceUnavailable)
		return
	}
	providerID := r.PathValue("providerID")
	modelID := r.PathValue("modelID")

	var req struct {
		SuccessorModelID string `json:"successor_model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	if err := h.ModelRegistry.DeprecateModel(r.Context(), providerID, modelID, req.SuccessorModelID); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	entry, err := h.ModelRegistry.Get(r.Context(), providerID, modelID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "entry": entry})
}
