package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/memory/store"
)

func (h *SysAdminHandler) HandleGetPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := h.SystemRepo.ListPreferences(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prefs)
}

func (h *SysAdminHandler) HandleSetPreference(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := h.SystemRepo.UpsertPreference(r.Context(), key, req.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot reload preference in Agent
	h.Agent.SetPreferences(map[string]string{key: req.Value})
	if h.Agent.Memory() != nil {
		if ic, ok := h.Agent.Memory().Working().Immutable().(*store.ImmutableCore); ok {
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "key": key, "value": req.Value})
}
