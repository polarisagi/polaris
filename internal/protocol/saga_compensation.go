package protocol

import (
	"context"
	"sync"
)

// ============================================================================
// Saga 补偿的单一事实源与结果回报通道（ADR-0088 决策一终态）
//
// 归属裁决：`ExecNode.Compensation` 是 Saga 补偿的**唯一**声明来源。
//
// 裁决依据（2026-08-06 全仓核查）：此前存在两套并行机制，但其中一套结构上是死的——
//
//	A. ExecNode.Compensation → DAGExecutor.runCompensation
//	   活的：validator.go 强制副作用节点（write_local/write_network）必填，
//	   携带**本次调用实际使用的参数**，且执行方知道哪些节点真正成功过。
//	B. ToolDefinition.UndoFn → sCtx.SagaLog → fsm.rollbackSaga
//	   死的：`UndoFn` 字段在全仓从未被赋值（tool.yaml 加载器无对应字段映射，
//	   唯一出现处是 agent_execute_dag.go 读取它）。因此 SagaLog 恒为空，
//	   rollbackSaga 恒遍历空切片、恒返回 S_ROLLBACK_OK——即 FSM 的 S_ROLLBACK
//	   自始至终没有反映过任何真实补偿结果。
//
// B 之所以必须删除而非"接线补活"：UndoFn 是挂在**工具定义**上的静态工具名，
// 没有参数绑定——补偿"删除刚创建的那个文件"需要知道是哪个文件，而工具定义
// 层面拿不到本次调用的实参。这是设计上的死胡同，不是接线缺口。
//
// 删除 B 后，FSM 的 S_ROLLBACK 不再自己执行补偿，改为**汇报** execute/dag 层
// 的补偿结果——这正是本文件 Recorder 的职责：DAG 层记录每次补偿的成败，
// FSM 据此在 S_ROLLBACK_OK / S_ROLLBACK_PARTIAL 之间选择。S_ROLLBACK 状态
// 本身保留（它有两条语义不同的出边：→S_REPLAN 带 replanCount Guard、
// →S_FAILED 触发 ESCALATE），只是职责从"执行者"变为"结果汇报者"。
// ============================================================================

// SagaCompensationRecord 一次补偿动作的执行记录。
type SagaCompensationRecord struct {
	ToolName string
	Err      error // nil 表示补偿成功
}

// SagaCompensationRecorder 收集一轮 DAG 执行中所有补偿动作的执行结果，
// 供 FSM 的 S_ROLLBACK 转移判定整体补偿是完全成功还是部分失败。
//
// 跨包传递走 context（CtxSagaRecorderKey）：execute/dag.Runner 是无状态工厂，
// 每次 Run 内部现构造 DAGExecutor，外部拿不到实例；而 DAGRunner 接口的签名
// 稳定性有明确设计约束（见 internal/agent/provider.go 该接口上方注释）。
// 补偿结果是"本次执行期间的旁路状态"，与已有的 CtxTaskIDKey/CtxTaintLevelKey
// 同类，走 context 是本仓既定 idiom。
//
// nil 接收者对所有方法安全（记录为 no-op，Outcome 返回零值），
// 未注入时等价于"无补偿发生"，不改变调用方控制流。
type SagaCompensationRecorder struct {
	mu      sync.Mutex
	records []SagaCompensationRecord
}

// NewSagaCompensationRecorder 创建一个空记录器。
func NewSagaCompensationRecorder() *SagaCompensationRecorder {
	return &SagaCompensationRecorder{}
}

// CtxSagaRecorderKey 是 SagaCompensationRecorder 的 context 传递键。
type CtxSagaRecorderKey struct{}

// SagaRecorderFromContext 从 ctx 取出补偿记录器；未注入时返回 nil。
func SagaRecorderFromContext(ctx context.Context) *SagaCompensationRecorder {
	if r, ok := ctx.Value(CtxSagaRecorderKey{}).(*SagaCompensationRecorder); ok {
		return r
	}
	return nil
}

// Record 记录一次补偿动作的执行结果。err 为 nil 表示该补偿成功。
func (r *SagaCompensationRecorder) Record(toolName string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, SagaCompensationRecord{ToolName: toolName, Err: err})
}

// Outcome 返回本轮补偿的汇总：已执行数与其中失败数。
func (r *SagaCompensationRecorder) Outcome() (executed, failed int) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		executed++
		if rec.Err != nil {
			failed++
		}
	}
	return executed, failed
}

// FirstError 返回第一条失败补偿的错误，全部成功时返回 nil。
// 供 FSM 在 S_ROLLBACK_PARTIAL 分支向上传播具体原因（ESCALATE 需要它）。
func (r *SagaCompensationRecorder) FirstError() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if rec.Err != nil {
			return rec.Err
		}
	}
	return nil
}
