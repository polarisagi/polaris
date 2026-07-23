# ADR-0068: 开放基准适配器架构 (Benchmark Adapter)

## 状态
Accepted

## 背景 (Context)
随着能力评测体系的发展，我们需要引入行业标准开放基准（如 τ-bench, Terminal-Bench 等）来补充自定义的 harness 评测，支持离线或本地化的 Tier-0 基准测试能力（满足 GD-14-001 规范）。
现有的 `internal/eval/harness` 已经具备了一套相对完整的生命周期结构：`EvalCase`, `EvalResult`, `Runner`, `RunnerImpl`, `TrajectoryTrace`。因此不需要重新发明执行器，只需引入适配器层将外部数据集映射为内部模型。
出于数据隐私和许可证合规性的考量，我们不会将外部的完整测试数据集打包在代码仓库中，而是由用户在运行时指定本地数据集路径。仓库内仅保留 2~3 条 fixture 数据作为单元测试和集成测试保障。

## 决策 (Decision)
我们将在 `internal/eval/harness` 基础之上增加一层 `benchmark` 适配层。主要决策如下：
1. **接口定义**：引入统一的 `BenchmarkAdapter` 接口，定义外部基准的一致性行为：
    - `Name()` 返回基准名称（例如 "t-bench"）。
    - `Load(datasetPath string) ([]harness.EvalCase, error)` 负责将给定的数据集读取并转换为 `harness.EvalCase`。
2. **重用执行引擎**：基准适配器只负责数据的加载和转换，评测的实际执行仍然统一走现有的 `RunnerImpl` 机制。
3. **数据集隔离**：实际数据集路径由外部驱动（如 CLI 参数），内部不再做默认打包和包含。
4. **范围约束**：
    - 本次主要实现 `τ-bench`（t-bench）的转换逻辑。
    - 对于 `Terminal-Bench`，目前由于外部数据格式还在确认中，只进行接口预注册登记（返回 `apperr.CodeUnimplemented`），暂时不实现具体转换细节，避免对未知格式进行臆测。

## 结果 (Consequences)
**正面影响**：
- 通过复用已有设施，快速实现了开放基准框架的接入，增强了模型的基准衡量指标丰富度。
- 保证了数据集的许可与隐私安全，符合合规策略。
- 架构的统一保证了测试输出报告格式一致，不会导致分析脚本的碎片化。

**负面影响**：
- 用户需要在运行时指定本地数据集路径，这增加了首次执行时的配置工作量。
