package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func (h *SysAdminHandler) HandleGetPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := h.SystemRepo.ListPreferences(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, prefs)
}

func (h *SysAdminHandler) HandleSetPreference(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	// 权限模式只认单一事实源 permission_mode：旧客户端仍写 computer_use_mode 时别名到新键，
	// 取值必须是三档之一（非法值直接拒绝，不落库）。
	if key == types.LegacyPrefComputerUseMode {
		key = types.PrefPermissionMode
	}
	if key == types.PrefPermissionMode {
		if _, ok := types.ParsePermissionMode(req.Value); !ok {
			httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "permission_mode must be default|auto_review|full_access"), http.StatusBadRequest)
			return
		}
	}

	err := h.SystemRepo.UpsertPreference(r.Context(), key, req.Value)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	// Hot reload preference in Agent（未注入时仅持久化，下次启动生效；不得对 nil 接口调用）
	if h.Agent == nil {
		httputil.WriteJSON(w, map[string]string{"status": "ok", "key": key, "value": req.Value})
		return
	}
	h.Agent.SetPreferences(map[string]string{key: req.Value})
	if h.Agent.Memory() != nil {
		if core := h.Agent.Memory().ImmutableCore(); core != nil {
			ic := core.Fields()
			switch key {
			case "system_prompt", "global_goal":
				ic.GlobalGoal = req.Value
			case "system_prompt_template":
				ic.SystemPromptTemplate = req.Value
			case "custom_instructions":
				ic.CustomInstructions = req.Value
			}
		}
	}

	httputil.WriteJSON(w, map[string]string{"status": "ok", "key": key, "value": req.Value})
}
