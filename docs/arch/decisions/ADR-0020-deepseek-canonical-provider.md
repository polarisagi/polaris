# ADR-0020: 确立 DeepSeek V4 为全系统默认核心模型（Canonical Provider）

- **状态**: Accepted
- **日期**: 2026-06-08
- **决策者**: 架构组
- **相关模块**: M01 (Inference), M05 (Memory), M09 (Self-Improvement), M12 (Eval)

## 上下文

Polaris 架构中包含了大量极其消耗 Token 的后台常驻任务，例如：记忆压缩 (Consolidation)、长期偏好演化 (DurativeMemory)、自动课程生成 (Auto-Curriculum)、离线评估回归 (Eval Harness) 和提示词优化 (PromptOptimizer)。
若使用海外头部大模型（如 Claude 4.6 / GPT-5，普遍定价在 $3-$15/1M token 即 20-100 RMB/1M token 级别），这些并发的后台任务将会带来极高的财务成本，从而逼迫我们在架构设计时对其运行频率进行残酷的削减（预算硬约束）。

然而，根据 DeepSeek V4 最新定价，其输入在命中上下文缓存时低至 **0.02元/百万 tokens**，未命中也仅为 1元~3元，输出在 2元~6元。这使得智能计算的成本断崖式下降到了竞争对手的 **1/50 到 1/1000**（尤其在缓存命中极高的 Agent 场景）。

## 决策

1. **全面将 DeepSeek V4 (Flash/Pro) 确立为 Polaris 的第一推荐（Tier-0 Default）及权威基准提供商。**
2. **释放认知预算**：放开 M05 记忆压缩与 M09 自进化等后台模块的频率限制。以往因成本顾虑而设置的繁琐的 Token 预算卡点逻辑将被大幅简化。
3. **彻底抛弃“省钱降频”设计**：对于后台 LLM-as-a-Judge 裁判、多代 Pareto 遗传算法、反射反思 (Reflection)，将默认采用全量或极高密度的频率进行，用算力密度换取智能演化的速率。

## 本地模型的“特权化”定位 (Challenge Accepted)

在确立 DeepSeek 为全系统默认大脑的同时，需明确指出：**本地模型 (Local-SLM) 并未失去意义，而是从“省钱的降级方案”转变成了“处理极端场景的高级特权”**。

在 99% 的场景下，所有设备（尤其是 8GB-24GB 的 Tier 0-2）应无脑拥抱 DeepSeek API。但在剩余 1% 的极端边界场景下，本地极小模型（或 Logic Collapse Python 技能产物，ADR-0026）依然在**仅限超高配置机器 (Tier-3 64GB)** 上保持其特权地位，以应对以下核心挑战：

1. **物理延迟极限 (Latency Wall)**：云端 API 存在 50-200ms 的物理 RTT 墙。对于要求 <10ms 亚秒级实时反馈的微决策（如 M4 中的 StepScorer 路由评估或高频 GUI 控制），极小规模（如 1.5B）的内存常驻本地模型依然是打破物理极限的唯一解。
2. **绝对的数据主权与物理隔离 (Air-gapped)**：对于处理金融、医疗或绝对核心商业源码的用户，数据的“物理不离境”要求高于一切。这是 M11 策略中维持 `local_only` 模式的根本原因。
3. **可用性灾备 (Availability Fallback)**：云服务不可避免地存在并发上限（如 DeepSeek 并发 500/2500）与网络不可用时段。本地模型作为生存套件 (Survival Kit) 在彻底断网时提供最后一道降级指令防线。

## 结论

极致的性价比从根本上改变了 Polaris Agent 架构的系统学特征：让”全天候高频后台反思与持续自我纠正”成为普通个人开发者用得起的核心能力。与此同时，对于物理隔离、微秒级延迟响应的极致追求，我们将专门收敛于高配置机器（Tier-3）的特权边界中。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-08 | 初稿，确立 DeepSeek V4 Flash/Pro 为 Tier-0 默认 |
| 2026-06-13 | 补充 V4 Pro 原生 extended thinking 集成：`ThinkingMode` 三档（Disabled/High/Max）通过 `reasoning_effort` + `thinking.type=”enabled”` 字段驱动；thinking 启用时温度强制 0；`reasoning_content` 多轮回传由 Adapter 负责提取。MCTS/BestOfN/多候选路由方案因 V4 Pro 原生 thinking 已覆盖其能力而废弃，见 ADR-0022。|
