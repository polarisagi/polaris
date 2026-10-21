# ADR-0115: Agent 自主内核：信任档位、模型驱动循环、模型自选、并行子智能体、实时模型目录

- **状态**: Accepted
- **日期**: 2026-10-18
- **决策者**: MrLaoLiAI（设计：Claude；实现：架构团队）
- **相关模块**: M01（`internal/llm`）/ M04（`internal/agent`）/ M07（`internal/tool`）/ M08（`internal/execute`）/ M09（`internal/learning`）/ M11（`internal/security`）/ M13（`internal/gateway`）
- **修订**: 修订 HE-5；推翻 ADR-0105 决策二反例「把花费决定权交给 LLM」；修订 ADR-0098 决策七（步数上限）与 ADR-0103 决策三（本地子 Agent 委派）

## 上下文

2026 年行业共识与基准研究表明：harness 决定的成绩差距可以显著大于模型本身（同模型换 harness 波动方差为换模型的 7.8 倍）。优秀的 agentic harness 遵循「薄控制、厚能力」——给予模型尽可能宽的行动空间，程序侧收敛于守住物理安全底线、持久化（State-in-DB）与预算监控。
Polaris 拥有完备的五防线、Taint 追踪、State-in-DB 与指标门控，但原有回合控制流是典型的「重脚手架」：固定 Perceive→Plan→Execute→Reflect→Respond 五段式，每回合 24 步硬截断、最多 3 次再规划；模型无法自主选模型和思考档，无法并行派生子智能体，中间数据反复进出上下文带来 token 浪费。此外，前端手选模型在 Agent 回合未生效（缺陷）。

为此，本决策确立 polaris 从「程序编排、模型填空」向「模型驱动、程序兜底」的自主内核演进，兼顾能力优先与 token 极致利用（T1~T8 原则）。

## 决策

### 一、核心架构原则

1. **能力原则（C1~C4）**：
   - **C1 模型决策，程序兜底**：回合内下一步行动、何时完成、使用何种模型由模型自主决定；程序负责生命周期管理、持久化（HE-6）、安全闸门、计量与终止恢复。
   - **C2 预算用时间和金额表达**：废除硬性步数截断；引入基于墙钟时间与 token/USD 成本的预算机制，辅以失控保护（runaway guard）。超预算时模型先行收尾并沉淀进度。
   - **C3 行动空间宁宽勿窄**：在 `full` 信任档下取消命令白名单、路径白名单与出站阻断限制，提供宿主直通环境。
   - **C4 验证器在循环外**：评测引擎、信任档位与 KillSwitch 独立于智能体主循环，模型在运行时只读，无法篡改。

2. **Token 原则（T1~T8）**：
   - **T1 单一稳定前缀**：请求前缀（system + 排序工具定义 + 技能索引）逐字节一致以最大化 KV-cache 命中；易变信息（时间、召回、用量）严格置于消息末尾。
   - **T2 历史只追加**：已发出历史消息禁止原位改写或乱序重排。
   - **T3 观察遮蔽代替 LLM 摘要**：超过 N 轮的陈旧工具大输出采用批量驱逐并替换为「一行摘要 + `read_tool_ref` 指针」，避免逐轮频繁修改前缀破坏缓存。
     - *2026-10-21 复核（ADR-0120 决策四）*：按 4 条一批遮蔽仍会频繁打断缓存，而命中价约为未命中价 1/50，小批遮蔽得不偿失。改为上下文达到触发阈值才遮蔽、只遮蔽大结果、一次遮蔽全部候选。
   - **T4 主会话不中途换模型或思考档**：换模型/思考档会导致整段前缀缓存失效；跨阶段模型切换优先通过派生子智能体完成，主会话切换提示缓存代价并在下一轮生效。
   - **T5 子智能体只交回结论**：子智能体执行轨迹与庞大输出留在子会话，仅向父会话交回提炼结论，全文卸载为引用。
   - **T6 中间数据不过上下文**：通过 code_act 工具桥使多步骤数据流转留在解释器内存，避免通过上下文往返。
   - **T7 模型可见计量**：每轮在易变上下文末尾注入实时 `tokens / cost / wall` 信息，实现模型自主成本感知。
   - **T8 后台任务走便宜模型 + 错峰**：记忆整理、自改进等非交互任务统一走 default 便宜池，关闭思考档并依 `pkg/offpeak` 错峰调度。

---

### 二、工作包决策摘要

#### 决策一（WP1 信任档位与快照回滚）
引入系统全局配置 `[security] trust_profile`（`full | standard | hardened`，默认 `standard` 严格保持现状行为），所有闸门通过纯函数 `security.ProfilePolicy(profile)` 统一判定。在 `full` 档下，bash/run_command 允许宿主直接执行，出站与 HITL 采用「记录+审计+高风险写前影子 git 快照」而非阻断；新增内置只读工具 `checkpoint_list` 与恢复工具 `checkpoint_restore`。
- *反例*: 禁止在非 `ProfilePolicy` 处散落 profile 条件分支；禁止在无快照保护的 full 档执行高危写。
- *重新评估*: 若用户在 full 档下的误删发生率高于预期，考虑增设不可豁免的系统级关键目录黑名单保护。
- *复核 2026-10-21（ADR-0121）*: 授权相关字段（`SandboxDisabled`/`AllowedPathsOpen`/`HITLAutoApprove`/`RunCmdWhitelistEnabled`/`CodeActAllToolsAllowed`）移出档位，改由每次调用的授权凭据派生；档位只保留权限上限（hardened ≤ auto_review）与非授权功能开关。理由：启动期档位与运行期 `permission_mode` 不联动，导致"完全访问"下仍被沙箱/白名单拒绝。原"关键目录黑名单"重新评估项已由 ADR-0121 决策四自保护路径落地。

#### 决策二（WP2 智能体循环内核）
新增 `loop_mode = "agentic"` 作为默认内核模式（原五段式标记为 `staged` 冻结），模型在单一工具循环（S_LOOP / S_LOOP_EXEC）内自主完成感知、调用、观察与收尾。无工具调用且 completion=auto 即为最终回复，显式模式下通过 `finish` 工具完成。每个工具调用严格通过 S_VALIDATE 四层校验链与 `ExecuteTool` 物理把关。支持通过网关消息入队实现回合内插话（steer）。
- *反例*: 禁止绕过 S_VALIDATE 或 ExecuteTool 直发工具执行；禁止在 FSM 内部执行阻塞性 IO。
- *重新评估*: 若复杂任务在 agentic 模式下的规划拆解失败率显著高于 staged 模式，由评测数据驱动触发针对性的规划工具挂载。

#### 决策三（WP3 模型自选、实时目录与手选锁定）
在路由层新增 `types.WithProviderRef(ref)` 支持按 `<provider_models 记录 ID 前缀>` 或 `<provider_id>/<model_id>` 引用精确寻址与按需实例化（LRU 缓存）。实现模型目录刷新器，周期性与错峰扫描已启用厂商 `/models` 接口持久化至 `provider_discovered_models` 表。提供 `list_models` 与 `switch_model` 工具。生效优先级：用户手选（`req.ModelID`）锁定 > 委派/角色指定 > 模型请求 > 阶段默认。修复用户手选模型未透传 Agent 回合缺陷。
- *反例*: 禁止跨厂商传递裸模型名导致 400（滥用 `WithModel`）；禁止在 `local_only_mode` 下解析云端模型引用。
- *重新评估*: 若按需实例化造成 Provider 内存激增（>32），调整 LRU 缓存规格并增设实例超时驱逐。

#### 决策四（WP4 并行子智能体与控制体系）
新增 `spawn_agents` 工具支持批量派生多个子智能体，支持 `wait="all"` 挂起等待与 `wait="none"` 后台异步，配套 `agent_status`、`agent_result`、`agent_message` 与 `agent_stop` 控制原语。子智能体结果超长截断并卸载为引用。本地子智能体默认允许再委派（`AllowDelegation=true`），并发与深度由 `delegation_chain_max_depth` 限制；跨节点委派保持严格禁止再委派（安全边界）。
- *反例*: 禁止跨节点委派任务继承或覆盖 `AllowDelegation=false`；禁止子智能体全文刷屏穿透主会话上下文。
- *重新评估*: 若并行子智能体争用共享项目资源导致冲突率增加，引入跨子智能体的文件写锁协议。

#### 决策五（WP5 行动空间扩展：code_act 工具桥与后台进程）
在 Python code_act 会话中通过独立管道注入 `polaris` 库，提供 `polaris.call` 与 `call_many`，子调用在内核端完整经过 `ExecuteTool`，Taint 污点严格并入执行结果（HE-2）。bash 工具扩展后台作业支持（`background: true`，返回 `job_id`，配套 `job_status/output/kill`）与持久化长驻会话（`session_id`），进程状态落盘。
- *反例*: 禁止 code_act 内调用工具绕过五防线或丢弃 Taint 标记；禁止在 standard/hardened 档放行副作用写工具给 code_act。
- *重新评估*: 若专用管道通信在受限容器环境下存在兼容性问题，提供基于本地命名管道（FIFO）的备用通道。

#### 决策六（WP6 自改进闭环与 sleep-time）
利用会话空闲（`sleep_time.idle_minutes`）与错峰窗口，基于便宜模型启动后台记忆提炼与失败模式聚类分析。开放只读 harness 覆盖层（`<DataDir>/harness_overlay/`，含技能、提示词追加、工具说明），任何变更必须在循环外的只读评测器上经 held-in / held-out 双重验证无退化后方可合并，并支持 git 版本回滚。评测器、信任档位与内核源码对模型不可写（在 standard/hardened 档下由 path_guard 与 ValidatePath 强制阻断；在 full 档下此隔离属于约定，不是物理安全边界，需配合系统权限控制）。
- *反例*: 禁止模型通过工具修改自身评测配置或提权配置；禁止未经双切验证直合覆盖层。
- *重新评估*: 若连续多次自改进未带来 held-out 分数提升，自动触发回退并降低改进提案频率。

#### 决策七（WP7 评测对比与描述性文档订正）
通过真实任务集与 mock benchmark 对比 staged 与 agentic 模式的完成率、调用开销与 token 消耗；对 `M04-Agent-Kernel.md` 与架构文档进行现状订正，保持规范与代码事实高度一致。

---

## 后果

- **正向**:
  - 全面释放前沿模型在复杂、长程任务中的自主推理与工具编排能力。
  - 通过单一工具循环、稳定前缀、批量观察遮蔽与解释器内部数据流转，大幅削减无谓的 LLM 调用与 token 开销。
  - 信任档位（`full`）让开发者能够在专用受控环境中获得与主机完全打通的开发体验，而快照回滚与外部评测机制提供安全安全网。
  - 用户界面模型手选能够真正生效，多模型协同与子任务并行化成为一等公民。
- **负向**:
  - `full` 档赋予模型极高系统权限，在非沙箱环境下对宿主文件系统具有直接读写能力，依赖影子快照机制兜底。
  - 核心循环与路由机制改造面较大，必须严格保证测试用例与历史兼容性（staged 模式平稳保留）。
- **反例守护**:
  - 驳回「模型可在运行时提权提升自己的 TrustProfile」。
  - 驳回「为了速度跳过 S_VALIDATE 或 ExecuteTool 五防线校验」。
  - 驳回「在 Go FSM 状态机锁内发起网络请求或大模型推理」。
  - 驳回「允许跨节点委派的任务开启二次委派」。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 完全无状态机包裹的自由 `while True` 循环 | 违反 HE-5/HE-6，破坏崩溃恢复、持久化审计、预算计量与外部中断响应能力。 |
| 维持程序固定阶段阶梯与步数截断 | 压抑模型能力边界（capability overhang），长任务频繁遭遇硬中断。 |
| 无影子快照的裸奔 full 档 | 误操作不可逆，高风险动作缺乏撤销手段，无法建立开发者信任。 |
| 跨厂商直接复用 `WithModel` 传递模型字符串 | 触发第三方适配器 400 校验错误；必须采用结构化 Provider 引用寻址。 |

## 引用代码

- `internal/security/policy/profile.go`
- `internal/agent/fsm/state_machine.go`
- `internal/agent/fsm/loop_*.go`
- `internal/agent/agent_loop*.go`
- `internal/llm/router.go`
- `internal/llm/modelcatalog/`
- `internal/tool/builtin/list_models/`
- `internal/tool/builtin/spawn_agents/`
- `internal/action/codeact/code_act.go`
- `internal/learning/sleep_time.go`
- `docs/arch/spec/state.yaml §thresholds.m4_kernel`

## 重新评估触发条件

1. `llm_calls` 统计中，agentic 模式由于工具循环过长导致单回合成本失控率 > 10% → 收紧 runaway_guard 默认值。
2. 发现模型在 full 档下的影子快照无法有效覆盖外部持久化影响（如外部数据库或云资源修改）→ 增强风险警告或对特定外部动作增加审批提示。
3. 厂商 /models 接口格式发生重大变更或新增强制私有认证协议 → 升级模型发现扫描器适配层。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-21 | T3 观察遮蔽由 ADR-0120 决策四修订为成本门控。 |
| 2026-10-18 | 初稿并 Accepted。整合 2026-10 背景调研，确立信任档位、模型驱动循环、模型自选、并行子智能体、code_act 工具桥与自改进闭环体系。 |
