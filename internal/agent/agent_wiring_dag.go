package agent

// DAG 执行引擎注入（2026-07-12 随 internal/execute 模块化新增，单独成文件
// 避免 agent_wiring.go 超过 R7 400 行上限，见 provider.go DAGRunner/DAGValidator
// 接口注释）。

// InjectDAGRunner 注入单 Agent 内工具链 DAG 执行引擎。生产路径由
// cmd/polaris/boot_agent.go 构造 execute/dag.Runner 注入；NewAgentWithDefaults
// 默认已注入，测试通常无需重复调用。
func (a *Agent) InjectDAGRunner(r DAGRunner) { a.dagRunner = r }

// InjectDAGValidator 注入 S_VALIDATE 四层校验管线。生产路径由
// cmd/polaris/boot_agent.go 构造 execute/dag.Validator 注入；NewAgentWithDefaults
// 默认已注入，测试通常无需重复调用。
func (a *Agent) InjectDAGValidator(v DAGValidator) { a.dagValidator = v }
