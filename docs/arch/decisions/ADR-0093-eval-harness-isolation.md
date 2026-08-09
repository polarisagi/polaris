# ADR-0093: M12 Eval Harness 评测隔离层设计

- **状态**: Accepted | **日期**: 2026-07-28（**2026-08-09 由 0051 改号至 0093**，见下）| **模块**: M12 `internal/eval/`

> **改号说明（2026-08-09）**：本 ADR 原编号 0051，与 `README.md`「已删除」表中的
> `ADR-0051 跨模块死代码清理与悬空接线收尾` 撞号。git 记录确认是编号复用而非笔误：
> 旧 0051 于 2026-07-28 14:46（commit `0fd9b38`）删除并入 ADR-0062，本 ADR 于次日
> 09:17（commit `02534e7`）建立并占用了刚释放的编号，违反 `README.md` 顶部与根
> `CLAUDE.md §文档可修订性`「三项不可变」中的**编号一经分配不复用**。
>
> 由本 ADR（后来者）让出编号而非改动已删除记录：全部历史引用——`ADR-0062` 合并
> 清单、`ADR-0053` 描述行、`local_playground/bake/202607*` 审计快照、多条 commit
> message——指的都是旧 0051；本 ADR 改号后，"ADR-0051"在全仓恢复单一含义。
> 改号代价已核实为零代码影响（全仓 `.go` 零引用，仅 README 两行 + 本文件标题）。

## 背景

Polaris 需要一个强隔离的评测系统来验证自进化 (M9) 生成的代码和规划。如果使用单一的集成测试流程，容易产生模型在验证集中作弊的情况。此架构必须确保评测的绝对独立性和数据集完整性，禁止 M9 模块直接修改评测策略。

## 决策一：Benchmark Runner 扩展（GD-14-007）

为使 Polaris 具备接入通用外部基准数据集（如 HumanEval、MBPP）能力，在 `internal/eval/harness` 之上扩展 Benchmark 机制。

- **隔离加载机制**：在 `internal/eval/benchmark` 包实现独立的公开数据集拉取逻辑，通过 HTTP 直接拉取 JSONL 文件进行解析。
- **协议兼容**：`protocol.EvalRunner` 新增 `RunBenchmarkDataset` 接口，在底层复用 `RunSuite` 的沙盒执行和评分体系（如 `EvalCase` 统一映射），而不干涉原 `training/validation` 存储链路。
- **入口编排**：运维端点 `POST /v1/eval/benchmark`（由 `EvalAdmin` 处理）通过查询参数 `dataset` 选择预设类型，异步触发 `RunBenchmarkDataset` 并立即返回 `StatusAccepted`，允许外部持续轮询执行状态。

## 反例守护

- **禁止直接绕过 Sandbox 执行 Benchmark Code**：即便数据集来自可信基准，所有代码评判必须使用 M12 原生 Sandbox Runner 执行，以避免任意代码执行 (RCE) 风险。
- **禁止修改 Benchmark 数据集元数据**：下载解析的数据应视为只读（Immutable），不能作为 `events` 追加回流到主数据库产生干扰。

## 引用代码

- `internal/eval/benchmark/benchmark.go`
- `internal/eval/harness/runner.go`
- `internal/gateway/server/sysadmin/evaladmin/admin.go`

> 2026-08-09 追记：重新评估触发条件——若出现真实需要"部分复用"训练数据与
> 基准数据集边界的场景（如需要用基准结果反哺训练），须先经安全评估确认不会
> 引入模型在验证集作弊风险，才重议"数据集只读不回流"边界；单纯为图方便的
> 边界放宽一律拒绝。
