package sysadmin

import (
	"net/http"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
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

	httputil.WriteJSON(w, allProfiles)
}
