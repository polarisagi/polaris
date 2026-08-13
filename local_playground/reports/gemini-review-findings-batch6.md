# 代码轨道 批次 6 审核报告

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-6-001 | P0 | internal/vfs | StageEphemeralFile 缺失 filename 路径穿越校验导致任意文件写突破沙箱隔离 | 高 | 是 |
| GR-6-002 | P1 | internal/execute/orchestrator | StateGraphExecutor 无条件多节点循环边被误入 AND-Join 硬依赖集合导致状态图死锁 | 高 | 是 |
| GR-6-003 | P2 | internal/vfs | provider.go 存留 2 行无符号声明空文件 | 高 | 是 |

置信度分布声明: 本批次所有 3 条发现均基于源码物理行精确排查与依赖路径核对（含路径穿越语法追踪、状态图图结构算法推演与代码搜检），且全部完成 §2-A 强制反证，无须假定运行时未知条件，故置信度均为高。

### [GR-6-001] StageEphemeralFile 缺失 filename 路径穿越校验导致任意文件写突破沙箱隔离
- 严重级: P0
- 模块: internal/vfs（层: L1）
- 位置: internal/vfs/workspace_manager_ephemeral.go:53
- 违反规则: HE-2 | HE-7
- 置信度: 高
- 可机械化: 是（建议规则: StageEphemeralFile 中传入 filename 必须经过 filepath.Base / resolveWithinRoot 校验，禁止直拼 filepath.Join）
- 反证: 已查 internal/action/codeact/code_act_script_staging.go:55/74、internal/swarm/planner/pool.go:162 及 workspace_manager_ephemeral.go:46-70 四处。code_act_script_staging.go:55 注释显式声明 "已在 vfs.WorkspaceManager.StageEphemeralFile 内部做路径穿越净化"，但 StageEphemeralFile 仅对 namespace 执行 filepath.Base 净化，对 filename 参数未经 filepath.Base 或 resolveWithinRoot 校验即直拼 filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename) 并在 L68 执行 os.WriteFile(full, data, 0700)。攻击者或 untrusted LLM 传入包含 ../.. 的 filename 可在宿主文件系统任意可写路径写入 0700 可执行文件，且该文件无法被 SweepEphemeralOrphans 巡检归还，构成物理隔离绕过。
- 问题: StageEphemeralFile 针对 namespace 做了 filepath.Base(filepath.Clean(namespace)) 过滤，但对 filename 未做任何路径穿越判定或 resolveWithinRoot 校验。直接拼接到 filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename) 会被 .. 相对路径解析击穿，允许在 wm.rootDir 甚至 ephemeralScriptsSubdir 之外的宿主任意目录创建可执行文件（0700）。上游 code_act_script_staging.go 误以为 VFS 内部已做防穿越而直接透传 filename，形成严重安全漏洞。
- 证据: 关键代码摘录（internal/vfs/workspace_manager_ephemeral.go:53）
  ```go
  func (wm *WorkspaceManager) StageEphemeralFile(namespace, filename string, data []byte) (absPath string, cleanup func(), err error) {
  	safeNS := filepath.Base(filepath.Clean(namespace))
  	...
  	full := filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename)
  ```
- 修复方向提示: 对 filename 统一应用 filepath.Base(filepath.Clean(filename)) 净化，或通过 resolveWithinRoot 进行范围边界校验。

### [GR-6-002] StateGraphExecutor 无条件多节点循环边被误入 AND-Join 硬依赖集合导致状态图死锁
- 严重级: P1
- 模块: internal/execute/orchestrator（层: L1）
- 位置: internal/execute/orchestrator/pattern_state_graph.go:300
- 违反规则: HE-5
- 置信度: 高
- 可机械化: 是（建议规则: 状态图构建 requiredPreds 时须剔除可达环路中的回边，不能仅过滤自环 From != To）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/execute/orchestrator/pattern_state_graph.go:249-316。initializeStateGraph 行 300 处构建 requiredPreds 时，仅通过 e.From != e.To 过滤了单节点自环，未过滤多节点环路（如 B -> C -> B）中的无条件反馈边。导致 requiredPreds["B"] 误记入 {"A": true, "C": true}。入口节点 A 执行完毕后，arriveJoin("B", "A") 判定已到达前驱 1/2 < 2，返回 false 拒绝触发 B；而 C 依赖 B 产出才能执行，造成 B 与 C 相互等待的运行时死锁。
- 问题: StateGraphExecutor 引入 AND-Join 语义以支持多前驱汇聚，但在提取无条件硬依赖前驱集合（requiredPreds）时，仅剔除了 e.From == e.To 的单节点自环，未排除多节点状态循环中的反馈回边（如 B -> C -> B 中的 C -> B）。导致节点在首次调度时误将未执行的下游反馈节点算作必须等待的前驱，引发永久死锁（死锁 — len(inFlight) == 0）。
- 证据: 关键代码摘录（internal/execute/orchestrator/pattern_state_graph.go:300）
  ```go
  if e.Condition == nil && e.From != e.To {
  	if requiredPreds[e.To] == nil {
  		requiredPreds[e.To] = make(map[string]bool, 2)
  	}
  	requiredPreds[e.To][e.From] = true
  }
  ```
- 修复方向提示: 在 initializeStateGraph 构建 requiredPreds 时，结合拓扑分析剔除从 To 到 From 可达的反馈回边，仅保留无环主干上的前驱依赖。

### [GR-6-003] provider.go 存留 2 行无符号声明空文件
- 严重级: P2
- 模块: internal/vfs（层: L1）
- 位置: internal/vfs/provider.go:1
- 违反规则: 维度G-死代码
- 置信度: 高
- 可机械化: 是（建议规则: 仅含 package 声明且无任何导出的源文件应清除）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/vfs/ 全包。provider.go 仅包含 2 行（package vfs 与空行），无任何类型、接口或函数声明，且全仓无任何对该文件的编译/构建依赖，属于清理遗留的空文件死代码。
- 问题: internal/vfs/provider.go 文件仅存 package vfs 声明，缺乏任何实质性代码或接口定义。作为历史重构遗留物，增加了模块目录的维护噪音。
- 证据: 源码全部内容（internal/vfs/provider.go:1）
  ```go
  package vfs

  ```
- 修复方向提示: 删除 internal/vfs/provider.go 文件。

## 已审文件清单
- internal/sandbox/argv_wrapper.go
- internal/sandbox/assign.go
- internal/sandbox/cmd_runner.go
- internal/sandbox/envelope.go
- internal/sandbox/extension_uninstall.go
- internal/sandbox/mock_proxy.go
- internal/sandbox/native_os_sandbox.go
- internal/sandbox/provider.go
- internal/sandbox/remote_sandbox.go
- internal/sandbox/sandbox_container.go
- internal/sandbox/sandbox_impl.go
- internal/sandbox/sandbox_inprocess.go
- internal/sandbox/sandbox_persistent.go
- internal/sandbox/sandbox_persistent_harness.go
- internal/sandbox/sandbox_persistent_session.go
- internal/sandbox/sandbox_router.go
- internal/sandbox/sysprocattr_linux.go
- internal/sandbox/sysprocattr_other.go
- internal/prompt/boundary.go
- internal/prompt/manager.go
- internal/prompt/prompt_builder.go
- internal/prompt/task_type.go
- internal/prompt/templates/templates.go
- internal/vfs/provider.go
- internal/vfs/vfs_other.go
- internal/vfs/vfs_unix.go
- internal/vfs/workspace_manager.go
- internal/vfs/workspace_manager_ephemeral.go
- internal/vfs/workspace_manager_quota.go
- internal/execute/dag/executor.go
- internal/execute/dag/executor_node.go
- internal/execute/dag/runner.go
- internal/execute/dag/taint_downgrade.go
- internal/execute/dag/validator.go
- internal/execute/orchestrator/agent_profile.go
- internal/execute/orchestrator/csv_fanout.go
- internal/execute/orchestrator/debate_worker.go
- internal/execute/orchestrator/default_worker.go
- internal/execute/orchestrator/mcp_a2a_worker.go
- internal/execute/orchestrator/orchestration.go
- internal/execute/orchestrator/pattern_dag.go
- internal/execute/orchestrator/pattern_debate.go
- internal/execute/orchestrator/pattern_mapreduce.go
- internal/execute/orchestrator/pattern_parallel.go
- internal/execute/orchestrator/pattern_sequential.go
- internal/execute/orchestrator/pattern_state_graph.go
- internal/execute/orchestrator/pattern_swarm.go
- internal/execute/orchestrator/pipeline.go
- internal/execute/orchestrator/pipeline_compensation.go
- internal/execute/orchestrator/reaper.go
- internal/execute/orchestrator/registry.go
- internal/execute/orchestrator/sqlite_blackboard.go
- internal/execute/orchestrator/sqlite_blackboard_hitl.go
- internal/execute/orchestrator/sqlite_blackboard_ops.go
- internal/execute/orchestrator/sqlite_blackboard_reaper.go
- internal/execute/orchestrator/sqlite_blackboard_utils.go
- internal/execute/orchestrator/state_graph_saga.go

## 明确未覆盖的范围
- 无

## 审了但无发现的模块
- internal/sandbox
- internal/prompt
