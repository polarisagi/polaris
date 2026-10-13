# internal/learning — 模块规范

> 架构文档：`docs/arch/M09-Self-Improvement-Engine.md`；跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

自进化引擎（L2）。三环：SurpriseIndex（惊讶度，`surprise/`）+ Reflexion（反思，`reflexion/`）+ LogicCollapse（技能固化，`logic_collapse_*.go`）；另有 `curriculum/` `synthetic/` `optimizer/`。消耗 Episodic 记忆，输出 Reflexion 结论（写回 memory）和固化技能（写回 skill store）。

## 权力边界 [MUST]

### 拥有
- SurpriseIndex 的计算与阈值判断权
- Reflexion 循环的发起权（异步，不阻塞主流程）
- Logic Collapse 的触发权（合成 Python 技能脚本）
- Eval Case 的生成与写入权（经注入的 EvalStore 接口）

### 禁止 [MUST NOT]
- 直接写入 `memory.db` 或 `agent` 相关表（写 memory 经注入的 MemoryWriter）
- 直接 import `internal/agent`（防循环）
- 在主流程同步路径调用 LLM（必须异步，不阻塞用户请求）
- 将未经安全审查的 Logic Collapse 输出直接部署为活跃技能（须过 M11 安全审查，XR-05）
- Reflexion 循环无限重试（必须有最大迭代次数）

## 消费端接口声明位置

`internal/learning/provider.go`：`EvalStore`、`LLMGenerator`、`MemoryWriter`、`SkillWriter`、`SurpriseMetrics`。新增外部依赖先在此声明，由 `cmd/polaris/boot_*.go` 注入。

## 异步执行约束

三环的后台协程必须经 `context.Context` 传播取消信号，禁止裸 `go func(){}()`，须经 `pkg/concurrent` 的 `SafeGo`（门控 `Test_inv_NoBareGoroutine`）。
