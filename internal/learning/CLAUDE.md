# internal/learning — 模块规范

> 对应架构文档：`docs/arch/M09-Self-Improvement-Engine.md`
> 跨模块规则：`docs/arch/Module-Dependency-Axioms.md §2`

## 模块定位

自进化引擎（Arch-L3）。驱动三环架构：
SurpriseIndex（惊讶度检测）+ Reflexion（反思与自我修正）+ LogicCollapse（技能固化）。
消耗 Episodic 记忆，输出 Reflexion 结论（写回 memory）和固化技能（写回 skill store）。

## 权力边界 [MUST]

### 拥有
- SurpriseIndex 的计算与阈值判断权
- Reflexion 循环的发起权（异步，不阻塞主流程）
- Logic Collapse 的触发权（合成 Python 技能脚本）
- Eval Case 的生成与写入权（通过注入的 EvalStore 接口）

### 禁止 [MUST NOT]
- **[MUST NOT]** 直接写入 `memory.db` 或 `agent` 相关表
  （写 memory 必须通过注入的 MemoryWriter 接口）
- **[MUST NOT]** 直接 import `internal/agent`（防循环）
- **[MUST NOT]** 在主流程同步路径上调用 LLM（LLM 调用必须异步，不阻塞用户请求）
- **[MUST NOT]** 将未经安全审查的 Logic Collapse 输出直接部署为活跃技能
  （必须通过 M11 安全审查，参考 XR-05）
- **[MUST NOT]** 在 Reflexion 循环中无限重试（必须有最大迭代次数上限）

## 消费端接口声明位置

`internal/learning/provider.go` — 已声明：EvalStore、LLMGenerator、MemoryWriter、
SkillWriter、SurpriseMetrics。
新增外部依赖时先在此文件声明接口，由 `bootstrap` 注入。

## 异步执行约束

三环中的任何后台工作协程，必须通过 `context.Context` 传播取消信号。
禁止裸 `go func(){}()`，必须通过受控的 goroutine 管理（参考 `pkg/concurrent/`）。
