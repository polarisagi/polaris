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
// 生产代码应优先使用 NewJITToken 包装函数。
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

// NewJITToken JIT 签发 Token。
// 签发后置到 Sandbox 门口: Planner(S_PLAN)→LLM决定调用→不签发Token(仅ToolIntent)
// → Gate1-5通过→JIT Mint Token(MaxCalls=1, TTL=5min)→立即拉起Sandbox
func NewJITToken(agentID, sessionID string, ops []TokenOperation, depth int, sandboxTier int) (*token.Token, error) {
	if depth >= 3 {
		return nil, ErrMaxDelegationDepth
	}
	return getTokenManager().Mint(agentID, opsToCapabilities(ops), sandboxTier, 5*time.Minute, 0)
}

// 委托链（子 Agent 请求比父级更受限的子 Token）机制已于 2026-07-14 移除：
// 该产品诉求已由 internal/execute/orchestrator 的 MaxSpawnDepth=3 任务派生深度
// 计数器独立覆盖（PostTask/PostBatch 前置校验），NewJITToken 当前生产调用点
// （agent_execute_dag.go）固定 depth=0 单层铸造，从未真正触发过跨层委托。
// 详见 docs/arch/M07-Tool-Action-Layer.md §4.6 更新说明。

// 哨兵错误统一改用 pkg/apperr（GR-Batch4 capability_token 修复）：原生 &TokenError{...}
// 未接入 apperr 体系会导致令牌提权/越权失败时 Trace 栈丢失上游上下文（apperr.CodeOf/IsCode
// 无法识别）。这里直接把哨兵变量本身的类型换成 *apperr.Error，而不是在每个 return 处再包一层
// apperr.Wrap——因为 NewJITToken 直接 `return nil, ErrMaxDelegationDepth` 返回这个哨兵，
// 调用方依赖的是"哨兵值本身被原样返回"（errors.Is 与 identity 比较均需成立），若在返回处
// 再包一层 apperr.Wrap 会产生新对象，破坏既有比较语义；直接让哨兵本身就是 *apperr.Error，
// 两种比较方式均不受影响，同时 apperr.IsCode(err, ...) 也能正确识别。
var (
	ErrTokenExpired       = apperr.New(apperr.CodeUnauthorized, "token expired")
	ErrMaxDelegationDepth = apperr.New(apperr.CodeResourceExhausted, "max delegation depth exceeded")
	ErrPolicyRevoked      = apperr.New(apperr.CodeForbidden, "policy revoked during execution")
)
