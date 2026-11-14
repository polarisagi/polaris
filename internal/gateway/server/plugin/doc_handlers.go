package plugin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TranslationTaskResponse task 响应固定格式（与 CONTRACT.md 完全一致）。
type TranslationTaskResponse struct {
	TaskID     string  `json:"task_id"`
	Status     string  `json:"status"`
	ErrorCode  *string `json:"error_code"`
	RetryAfter *int    `json:"retry_after"`
}

// translationRequest POST /v1/plugins/documentation/translations 请求体。
type translationRequest struct {
	CatalogID    string `json:"catalog_id"`
	Locale       string `json:"locale"`
	SourceSHA256 string `json:"source_sha256"`

	// 严防客户端注入任何正文、模型密钥、URL 或本地路径
	Markdown string `json:"markdown,omitempty"`
	Content  string `json:"content,omitempty"`
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	ModelKey string `json:"model_key,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

// HandleGetPluginDocumentation GET /v1/plugins/documentation
// 只读获取说明文档，不产生 LLM 计费；验证 catalog_id 存在。
func (h *PluginHandler) HandleGetPluginDocumentation(w http.ResponseWriter, r *http.Request) {
	catalogID := strings.TrimSpace(r.URL.Query().Get("catalog_id"))
	if catalogID == "" {
		httputil.RespondError(w, "catalog_id is required", nil, http.StatusBadRequest)
		return
	}

	locale := strings.TrimSpace(r.URL.Query().Get("locale"))
	if locale == "" {
		locale = "zh-CN"
	}
	if locale != "zh-CN" && locale != "en" {
		httputil.RespondError(w, "unsupported locale: "+locale, nil, http.StatusBadRequest)
		return
	}

	if h.DocLocator == nil {
		httputil.RespondError(w, "documentation locator not initialized", nil, http.StatusServiceUnavailable)
		return
	}

	// 验证 catalog_id 是否真实存在于系统（目录或实例）中，非真实存在的条目返回 404
	if !h.extensionExists(r.Context(), catalogID) {
		httputil.RespondError(w, "extension not found: "+catalogID, nil, http.StatusNotFound)
		return
	}

	auth := authcontext.FromContext(r.Context())
	resp, err := h.DocLocator.ResolveDocumentation(r.Context(), catalogID, locale, auth.UserID)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("plugin_doc_handler: failed to encode response", "err", err)
	}
}

// HandleCreatePluginTranslation POST /v1/plugins/documentation/translations
// 显式触发自定义扩展翻译任务。内置来源强制拒绝（403 builtin_translation_managed）。
func (h *PluginHandler) HandleCreatePluginTranslation(w http.ResponseWriter, r *http.Request) {
	if h.DocTranslations == nil {
		httputil.RespondError(w, "translation service not initialized", nil, http.StatusServiceUnavailable)
		return
	}

	req, err := parseTranslationRequest(r)
	if err != nil {
		httputil.RespondError(w, err.Error(), nil, http.StatusBadRequest)
		return
	}

	auth := authcontext.FromContext(r.Context())
	task, err := h.DocTranslations.EnqueueTranslation(r.Context(), auth.UserID, req.CatalogID, req.Locale, req.SourceSHA256)
	if err != nil {
		handleEnqueueTranslationError(w, err)
		return
	}

	taskResp := buildTaskResponse(task)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(taskResp); err != nil {
		slog.Warn("plugin_doc_handler: failed to encode response", "err", err)
	}
}

func parseTranslationRequest(r *http.Request) (*translationRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "invalid request body")
	}

	var req translationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "invalid json body")
	}

	// 严防注入
	if req.Markdown != "" || req.Content != "" || req.URL != "" || req.Path != "" || req.ModelKey != "" || req.APIKey != "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "client-provided content, model or paths are strictly rejected")
	}

	req.CatalogID = strings.TrimSpace(req.CatalogID)
	req.SourceSHA256 = strings.TrimSpace(req.SourceSHA256)
	if req.CatalogID == "" || req.SourceSHA256 == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "catalog_id and source_sha256 are required")
	}

	if req.Locale == "" {
		req.Locale = "zh-CN"
	}
	if req.Locale != "zh-CN" {
		return nil, apperr.New(apperr.CodeInvalidInput, "unsupported locale: "+req.Locale)
	}
	return &req, nil
}

func handleEnqueueTranslationError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if strings.Contains(msg, "builtin_translation_managed") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": "builtin_translation_managed",
			"code":  "builtin_translation_managed",
		}); err != nil {
			slog.Warn("plugin_doc_handler: failed to encode response", "err", err)
		}
		return
	}
	if strings.Contains(msg, "source_changed") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": "source_changed",
			"code":  "source_changed",
		}); err != nil {
			slog.Warn("plugin_doc_handler: failed to encode response", "err", err)
		}
		return
	}
	if strings.Contains(msg, "not found") {
		httputil.RespondError(w, "source document not found", err, http.StatusNotFound)
		return
	}
	httputil.RespondError(w, "", err, http.StatusInternalServerError)
}

// HandleGetPluginTranslationTask GET /v1/plugins/documentation/translations?task_id=...
// 读取指定任务的执行状态，严格主体隔离。
func (h *PluginHandler) HandleGetPluginTranslationTask(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID == "" {
		httputil.RespondError(w, "task_id is required", nil, http.StatusBadRequest)
		return
	}

	if h.DocTranslations == nil {
		httputil.RespondError(w, "translation service not initialized", nil, http.StatusServiceUnavailable)
		return
	}

	auth := authcontext.FromContext(r.Context())
	task, err := h.DocTranslations.GetTask(r.Context(), auth.UserID, taskID)
	if err != nil {
		if apperr.CodeOf(err) == apperr.CodeNotFound {
			httputil.RespondError(w, "task not found", nil, http.StatusNotFound)
			return
		}
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	taskResp := buildTaskResponse(task)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(taskResp); err != nil {
		slog.Warn("plugin_doc_handler: failed to encode response", "err", err)
	}
}

func (h *PluginHandler) extensionExists(ctx context.Context, catalogID string) bool {
	if h.ExtRepo == nil {
		return false
	}
	entry, err := h.ExtRepo.GetCatalogEntry(ctx, catalogID)
	if err == nil && entry != nil {
		return true
	}
	instances, err := h.ExtRepo.ListInstances(ctx)
	if err == nil {
		for _, inst := range instances {
			if inst.CatalogID == catalogID || inst.ID == catalogID {
				return true
			}
		}
	}
	return false
}

func buildTaskResponse(task *types.ExtDocTranslationTaskRow) TranslationTaskResponse {
	var errCode *string
	if task.ErrorCode != "" {
		c := task.ErrorCode
		errCode = &c
	}
	var retryAfter *int
	if task.RetryAfter > 0 {
		r := task.RetryAfter
		retryAfter = &r
	}
	return TranslationTaskResponse{
		TaskID:     task.TaskID,
		Status:     task.Status,
		ErrorCode:  errCode,
		RetryAfter: retryAfter,
	}
}
