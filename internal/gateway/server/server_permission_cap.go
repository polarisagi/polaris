package server

import (
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

// deploymentMaxPermissionMode 当前信任档位对应的权限上限（ADR-0121 决策一）。
func deploymentMaxPermissionMode() types.PermissionMode {
	prof := policy.ProfileStandard
	if cfg := config.Get(); cfg != nil && cfg.Security.TrustProfile != "" {
		prof = policy.TrustProfile(cfg.Security.TrustProfile)
	}
	return policy.ProfilePolicy(prof).MaxPermissionMode
}
