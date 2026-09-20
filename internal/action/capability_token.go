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

// maxCallsFromOps 取 ops 中最严格（最小非零）的 MaxCalls 作为令牌的单任务调用上限。
// 全部为 0（未声明）时返回 0，即 TokenClaims 语义下的"无限制"。
//
// 最小权限原则：与 TokenManager.minTTL 取最短 TTL 同构——一个令牌覆盖多个操作时，
// 约束取各操作中最紧的那个，而不是最松的。
func maxCallsFromOps(ops []TokenOperation) int {
	minCalls := 0
	for _, op := range ops {
		if op.MaxCalls <= 0 {
			continue
		}
		if minCalls == 0 || op.MaxCalls < minCalls {
			minCalls = op.MaxCalls
		}
	}
	return minCalls
}

// NewJITToken JIT 签发 Token。
// 签发后置到 Sandbox 门口: Planner(S_PLAN)→LLM决定调用→不签发Token(仅ToolIntent)
// → Gate1-5通过→JIT Mint Token(MaxCalls 取自 ops, TTL=5min)→立即拉起Sandbox
//
// MaxCalls 透传修复（2026-08-06）：此前这里对 Mint 硬编码 maxCallsPerTask=0，
// 而 TokenClaims.MaxCallsPerTask 的 0 语义是**无限制**（见该字段注释）。
// 调用方（agent_execute_dag.go code_act 分支）明明传了
// TokenOperation{MaxCalls: 1}，opsToCapabilities 却只取 ToolName、把 MaxCalls
// 整个丢弃——导致 M07 §4.6 与本函数注释三处声称的"一次性令牌"从未真正生效，
// 实际铸出的是 5 分钟内可无限次复用的令牌。令牌一旦泄漏（如被注入的 Agent
// 转手给其它节点），无限次调用与单次调用的风险差异是数量级的。
// 签名清理（2026-08-06）：移除 sessionID 与 depth 两个参数。
//   - sessionID 自引入起从未被使用过（令牌 claims 里没有会话维度，
//     TokenClaims 只有 AgentID）；
//   - depth 的委托链校验已于 2026-07-14 移除（见下方注释），唯一生产调用点
//     固定传 0，`depth >= 3` 分支结构上不可达。留着两个"看起来在做事、
//     实际不做事"的参数，会让调用方误以为委托深度在此处被校验。
func NewJITToken(agentID string, ops []TokenOperation, sandboxTier int) (*token.Token, error) {
	tok, err := getTokenManager().Mint(agentID, opsToCapabilities(ops), sandboxTier, 5*time.Minute, maxCallsFromOps(ops))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "capability_token: JIT Mint 失败", err)
	}
	return tok, nil
}

// 委托链（子 Agent 请求比父级更受限的子 Token）机制已于 2026-07-14 移除：
// 该产品诉求已由 internal/execute/orchestrator 的 MaxSpawnDepth=3 任务派生深度
// 计数器独立覆盖（PostTask/PostBatch 前置校验），NewJITToken 当前生产调用点
// （agent_execute_dag.go）固定 depth=0 单层铸造，从未真正触发过跨层委托。
// 详见 docs/arch/M07-Tool-Action-Layer.md §4.6 更新说明。

// ErrTokenExpired / ErrPolicyRevoked 已删除（2026-09-20，GR-4.2-009）：全仓零引用——
// 令牌过期的现役哨兵是 internal/security/token.ErrTokenExpired（Verify 路径返回），
// "执行中策略撤销"从未有返回点。ErrMaxDelegationDepth 已于 2026-08-06 随委托链一并删除。
