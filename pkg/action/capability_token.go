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

var GlobalTokenManager *policy.TokenManager

func init() {
	GlobalTokenManager, _ = policy.NewTokenManager()
}

func opsToCapabilities(ops []TokenOperation) []policy.CapabilityType {
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
	return caps
}

func intersectCapabilities(a, b []policy.CapabilityType) []policy.CapabilityType {
	var res []policy.CapabilityType
	m := make(map[policy.CapabilityType]bool)
	for _, capA := range a {
		m[capA] = true
	}
	for _, capB := range b {
		if m[capB] {
			res = append(res, capB)
			m[capB] = false
		}
	}
	return res
}

// NewJITToken JIT 签发 Token。
// 签发后置到 Sandbox 门口: Planner(S_PLAN)→LLM决定调用→不签发Token(仅ToolIntent)
// → Gate1-5通过→JIT Mint Token(MaxCalls=1, TTL=5min)→立即拉起Sandbox
func NewJITToken(agentID, sessionID string, ops []TokenOperation, depth int, sandboxTier int) (*policy.Token, error) {
	if depth >= 3 {
		return nil, ErrMaxDelegationDepth
	}
	return GlobalTokenManager.Mint(agentID, opsToCapabilities(ops), sandboxTier, 5*time.Minute)
}

// ValidateDelegation 校验委托链。
// 规则1 权限收缩: effectiveCapability = min(caller, target)
// 规则2 沙箱单调: target.SandboxTier >= caller.SandboxTier
// 规则3 溯源: DerivationDepth >= 3 → 拒绝
func ValidateDelegation(parentToken *policy.Token, parentDepth int, agentID, sessionID string, ops []TokenOperation, targetSandboxTier int) (*policy.Token, error) {
	if parentDepth >= 2 {
		return nil, ErrMaxDelegationDepth
	}
	if err := GlobalTokenManager.Verify(parentToken); err != nil {
		return nil, ErrTokenInvalid
	}

	// 规则2: 沙箱单调 (target 隔离不得弱于 caller，数字越大隔离越强)
	if targetSandboxTier < parentToken.Claims.SandboxTier {
		return nil, ErrSandboxTierEscalation
	}

	requestedCaps := opsToCapabilities(ops)
	effectiveCaps := intersectCapabilities(parentToken.Claims.Caps, requestedCaps)

	if len(effectiveCaps) == 0 {
		return nil, ErrCapabilityInsufficient
	}

	// Sign sub token
	return GlobalTokenManager.Mint(agentID, effectiveCaps, targetSandboxTier, 5*time.Minute)
}

var (
	ErrTokenExpired           = &TokenError{"token expired"}
	ErrTokenInvalid           = &TokenError{"token invalid"}
	ErrMaxDelegationDepth     = &TokenError{"max delegation depth exceeded"}
	ErrPolicyRevoked          = &TokenError{"policy revoked during execution"}
	ErrCapabilityInsufficient = &TokenError{"capability intersection is empty"}
	ErrSandboxTierEscalation  = &TokenError{"sandbox tier escalation is forbidden"}
)

type TokenError struct{ msg string }

func (e *TokenError) Error() string { return e.msg }
