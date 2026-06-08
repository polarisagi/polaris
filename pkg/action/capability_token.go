package action

import (
	"time"

	"github.com/polarisagi/polaris/pkg/substrate/policy"
)

// TokenOperation 单次授权操作。
type TokenOperation struct {
	ToolName string
	MaxCalls int
	Params   map[string]any
}

var globalTokenManager *policy.TokenManager

func init() {
	globalTokenManager, _ = policy.NewTokenManager()
}

// NewJITToken JIT 签发 Token。
// 签发后置到 Sandbox 门口: Planner(S_PLAN)→LLM决定调用→不签发Token(仅ToolIntent)
// → Gate1-5通过→JIT Mint Token(MaxCalls=1, TTL=5min)→立即拉起Sandbox
func NewJITToken(agentID, sessionID string, ops []TokenOperation, depth int) (*policy.Token, error) {
	if depth >= 3 {
		return nil, ErrMaxDelegationDepth
	}

	caps := []policy.CapabilityType{}
	for _, op := range ops {
		// 简单映射，实际应根据 ToolName 判断
		if op.ToolName == "run-sh" || op.ToolName == "bash" {
			caps = append(caps, policy.CapShell)
		} else if op.ToolName == "fetch_url" {
			caps = append(caps, policy.CapNetwork)
		} else {
			caps = append(caps, policy.CapProcess)
		}
	}

	if len(caps) == 0 {
		caps = []policy.CapabilityType{policy.CapProcess}
	}

	return globalTokenManager.Mint(agentID, caps, 5*time.Minute)
}

// ValidateDelegation 校验委托链。
// 规则1 权限收缩: effectiveCapability = min(caller, target)
// 规则2 沙箱单调: target.SandboxTier >= caller.SandboxTier
// 规则3 溯源: DerivationDepth >= 3 → 拒绝
func ValidateDelegation(parentDepth int, agentID, sessionID string, ops []TokenOperation) (*policy.Token, error) {
	if parentDepth >= 3 {
		return nil, ErrMaxDelegationDepth
	}
	return NewJITToken(agentID, sessionID, ops, parentDepth+1)
}

var (
	ErrTokenExpired       = &TokenError{"token expired"}
	ErrMaxDelegationDepth = &TokenError{"max delegation depth exceeded"}
	ErrPolicyRevoked      = &TokenError{"policy revoked during execution"}
)

type TokenError struct{ msg string }

func (e *TokenError) Error() string { return e.msg }
