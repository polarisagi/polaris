package agent

import (
	"github.com/polarisagi/polaris/internal/execute/dag"
)

// 本文件承载「仅测试可达」的构造器。
// 2026-08-08：NewAgentWithDefaults 原在 agent.go，是 internal/agent 生产代码
// 直接 import internal/execute/dag 的唯一原因，与 provider.go §DAGRunner
// 「禁止：agent 直接 import internal/execute/dag」自相矛盾。移入 _test.go 后
// 该 import 从生产构建中消失，provider.go 的消费端接口约定才真正成立。
// NewAgentWithDefaults 构造带默认依赖的 Agent，主要供测试/开发场景使用。
//
// dagRunner/dagValidator 默认注入 execute/dag.Runner/Validator（唯二在此处直接
// import internal/execute/dag 的位置，其余 agent 包代码一律通过 provider.go 的
// DAGRunner/DAGValidator 接口消费）。二者均为无状态适配器（零字段），此处构造
// 纯粹是为了让 NewAgentWithDefaults 保持"开箱即可跑通完整 FSM S_EXECUTE/
// S_VALIDATE 路径"的既有约定（历史上 dag 是 agent 同目录子包，测试从不需要
// 显式注入）；生产路径 cmd/polaris/boot_agent.go 的 buildAgent 会显式调用
// InjectDAGRunner/InjectDAGValidator 覆盖此处默认值，语义不变。
func NewAgentWithDefaults(id string) *Agent {
	a := NewAgent(id, nil, nil)
	a.dagRunner = dag.NewRunner()
	a.dagValidator = dag.NewValidator()
	return a
}
