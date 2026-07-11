package action

import (
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"

	"sync"
	"time"
)

// TokenOperation 单次授权操作。
type TokenOperation struct {
	ToolName string
	MaxCalls int
	Params   map[string]any
}

// getTokenManager 返回进程级 TokenManager 单例（sync.OnceValue 惰性初始化）。
// 初始化失败时 panic（fail-fast），避免 nil 静默传播到安全校验路径。
// 使用 sync.OnceValue 而非 var + init()：无包级可变状态，初始化顺序更清晰。
var getTokenManager = sync.OnceValue(func() *token.TokenManager {
	tm, err := token.NewTokenManager()
	if err != nil {
		// TokenManager 是核心安全基础设施，初始化失败属于不可恢复错误。
		panic("action: failed to initialize token manager: " + err.Error())
	}
	return tm
})

// GetTokenManager 返回进程级 TokenManager。
// 工具层校验（validateToken）和单元测试 Mint 通过此函数访问。
// 生产代码应优先使用 NewJITToken / ValidateDelegation 包装函数。
func GetTokenManager() *token.TokenManager {
	return getTokenManager()
}

func opsToCapabilities(ops []TokenOperation) []token.CapabilityType {
	caps := []token.CapabilityType{}
	for _, op := range ops {
		// 简单映射，实际应根据 ToolName 判断
		switch op.ToolName {
		case "run-sh", "bash":
			caps = append(caps, token.CapShell)
		case "fetch_url":
			caps = append(caps, token.CapNetwork)
		default:
			caps = append(caps, token.CapProcess)
		}
	}

	if len(caps) == 0 {
		caps = []token.CapabilityType{token.CapProcess}
	}
	return caps
}

func intersectCapabilities(a, b []token.CapabilityType) []token.CapabilityType {
	var res []token.CapabilityType
	m := make(map[token.CapabilityType]bool)
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
func NewJITToken(agentID, sessionID string, ops []TokenOperation, depth int, sandboxTier int) (*token.Token, error) {
	if depth >= 3 {
		return nil, ErrMaxDelegationDepth
	}
	return getTokenManager().Mint(agentID, opsToCapabilities(ops), sandboxTier, 5*time.Minute, 0)
}

// ValidateDelegation 校验委托链。
// 规则1 权限收缩: effectiveCapability = min(caller, target)
// 规则2 沙箱单调: target.SandboxTier >= caller.SandboxTier
// 规则3 溯源: DerivationDepth >= 3 → 拒绝
func ValidateDelegation(parentToken *token.Token, parentDepth int, agentID, sessionID string, ops []TokenOperation, targetSandboxTier int) (*token.Token, error) {
	if parentDepth >= 2 {
		return nil, ErrMaxDelegationDepth
	}
	if err := getTokenManager().Verify(parentToken); err != nil {
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
	return getTokenManager().Delegate(parentToken, agentID, effectiveCaps, targetSandboxTier, 5*time.Minute)
}

// 哨兵错误统一改用 pkg/apperr（GR-Batch4 capability_token 修复）：原生 &TokenError{...}
// 未接入 apperr 体系会导致令牌提权/越权失败时 Trace 栈丢失上游上下文（apperr.CodeOf/IsCode
// 无法识别）。这里直接把哨兵变量本身的类型换成 *apperr.Error，而不是在每个 return 处再包一层
// apperr.Wrap——因为 ValidateDelegation/NewJITToken 直接 `return nil, ErrXxx` 返回这些哨兵，
// 现有调用方（含 inv_test.go 的 errors.Is(err, ErrMaxDelegationDepth) 和
// capability_token_test.go 的 err != ErrSandboxTierEscalation 严格等值比较）依赖的是"哨兵值
// 本身被原样返回"，若在返回处再包一层 apperr.Wrap 会产生新对象，破坏这些既有比较语义；
// 直接让哨兵本身就是 *apperr.Error，两种比较方式（identity / errors.Is）均不受影响，
// 同时 apperr.IsCode(err, ...) 也能正确识别。
var (
	ErrTokenExpired           = apperr.New(apperr.CodeUnauthorized, "token expired")
	ErrTokenInvalid           = apperr.New(apperr.CodeUnauthorized, "token invalid")
	ErrMaxDelegationDepth     = apperr.New(apperr.CodeResourceExhausted, "max delegation depth exceeded")
	ErrPolicyRevoked          = apperr.New(apperr.CodeForbidden, "policy revoked during execution")
	ErrCapabilityInsufficient = apperr.New(apperr.CodeForbidden, "capability intersection is empty")
	ErrSandboxTierEscalation  = apperr.New(apperr.CodeForbidden, "sandbox tier escalation is forbidden")
)
