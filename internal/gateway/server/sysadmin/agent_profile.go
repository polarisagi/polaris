package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
)

// HandleListAgentProfiles returns all registered AgentProfiles.
func (h *SysAdminHandler) HandleListAgentProfiles(w http.ResponseWriter, r *http.Request) {
	paths := orchestrator.DefaultAgentProfilePaths()
	var allProfiles []orchestrator.AgentProfile
	
	for _, path := range paths {
		if profiles, err := orchestrator.ListAgentProfiles(path); err == nil {
			allProfiles = append(allProfiles, profiles...)
		}
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allProfiles)
}
