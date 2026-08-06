package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// SagaCompensationLedger 记录本次执行中已经真实执行过的 Saga 补偿动作，
// 用于跨"两条独立补偿路径"去重。
//
// 为什么需要它（2026-08-06 一致性审查发现）：系统里存在两套并行、数据来源
// 互不相干的 Saga 补偿，同一次执行失败会各跑一遍——
//
//	A. internal/execute/dag：补偿动作来自 DAG 节点声明的 node.Compensation
//	   （LLM 生成计划时填写，S_VALIDATE 校验管线强制副作用节点必填），
//	   在 DAGExecutor.runCompensation 中逆序同步执行。
//	B. internal/agent/fsm：补偿动作来自工具注册表的 toolDef.UndoFn
//	   （agent_execute_dag.go 在每次工具调用成功后追加进 sCtx.SagaLog），
//	   在 S_EXECUTE → S_ROLLBACK 转移的 rollbackSaga Effect 中逆序执行。
//
// 一个既在 DAG 里声明了 Compensation、又在注册表里登记了 UndoFn 的副作用工具，
// 其 undo 会被执行两次。对"删除文件 / 退款 / 撤回消息"这类**非幂等**补偿，
// 这是数据损坏级缺陷（第二次 undo 作用在已经被撤销的状态上）。
//
// 本类型是止血措施而非终局：两层补偿的职责归属需要单独 ADR 裁决（合并到
// 单一 SSoT，还是明确划分各自负责的副作用类别）。在那之前，用"同一补偿动作
// 只执行一次"这条不变量把数据损坏风险关掉。
//
// 去重键刻意用 (工具名 + 参数哈希) 而非节点 ID：两条路径对"同一个补偿"的
// 命名不同——A 用 node.Compensation.ToolName，B 用 toolDef.UndoFn，节点 ID
// 在 B 侧甚至是用 toolName 顶替的（见 agent_execute_dag.go SagaLog 追加处）。
// 只有"最终会执行哪个工具、带什么参数"这个语义在两侧是一致的。反过来，
// 若两侧的 undo 工具或参数确实不同，说明是两个语义不同的补偿动作，
// 就应该都执行——本键的粒度恰好表达了这个判断。
type SagaCompensationLedger struct {
	mu   sync.Mutex
	done map[string]struct{}
}

// NewSagaCompensationLedger 创建一个空账本。零值 *SagaCompensationLedger（nil）
// 也可安全使用：所有方法对 nil 接收者退化为"不去重"，与引入本机制前行为一致。
func NewSagaCompensationLedger() *SagaCompensationLedger {
	return &SagaCompensationLedger{done: make(map[string]struct{})}
}

// CtxSagaLedgerKey 是 SagaCompensationLedger 的 context 传递键。
//
// 用 context 而非扩展 DAGRunner 接口签名：execute/dag.Runner 是无状态工厂，
// 每次 Run 内部现构造 DAGExecutor，外部拿不到实例；而 DAGRunner 接口的签名
// 稳定性有明确设计约束（见 internal/agent/provider.go 该接口上方注释——
// 参数必须保持匿名函数类型，否则 execute/dag 需反向 import agent 包）。
// 账本是"本次执行期间的旁路协调状态"，与已有的 CtxTaskIDKey/CtxTaintLevelKey
// 同类，走 context 是本仓既定 idiom。
type CtxSagaLedgerKey struct{}

// SagaLedgerFromContext 从 ctx 取出补偿账本；未注入时返回 nil
// （nil 账本的 TryClaim 恒返回 true，即不去重）。
func SagaLedgerFromContext(ctx context.Context) *SagaCompensationLedger {
	if l, ok := ctx.Value(CtxSagaLedgerKey{}).(*SagaCompensationLedger); ok {
		return l
	}
	return nil
}

// SagaCompensationKey 计算补偿动作的去重键。
func SagaCompensationKey(toolName string, args []byte) string {
	sum := sha256.Sum256(args)
	return toolName + ":" + hex.EncodeToString(sum[:8])
}

// TryClaim 尝试认领一个补偿动作的执行权。
// 首次调用返回 true（调用方应执行该补偿）；此后对同一键返回 false。
//
// nil 接收者恒返回 true——未注入账本时不改变原有行为（fail-open）。
// 这里刻意 fail-open 而非 fail-closed：账本缺失时"多补偿一次"是回到修复前的
// 已知状态，而"漏补偿"会留下未回滚的副作用，后者更糟。
func (l *SagaCompensationLedger) TryClaim(toolName string, args []byte) bool {
	if l == nil {
		return true
	}
	key := SagaCompensationKey(toolName, args)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done == nil {
		l.done = make(map[string]struct{})
	}
	if _, seen := l.done[key]; seen {
		return false
	}
	l.done[key] = struct{}{}
	return true
}

// Count 返回已认领的补偿动作数（观测/测试用）。
func (l *SagaCompensationLedger) Count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.done)
}
