# ADR-0068: 开放基准适配器架构（Benchmark Adapter）

- **状态**: Accepted（已执行）| **日期**: 2026-07-23 | **模块**: `internal/eval/harness`

## 决策

引入 `BenchmarkAdapter` 接口（`Name()`/`Load(datasetPath) ([]harness.EvalCase, error)`）将外部开放基准（τ-bench/Terminal-Bench）数据集映射为内部 `EvalCase`，复用既有 `RunnerImpl` 执行引擎，不重新发明执行器。数据集路径由用户运行时指定，不打包进仓库（隐私/许可合规），仓库仅留 2~3 条 fixture。本次仅实现 τ-bench 转换逻辑；Terminal-Bench 因外部格式未定，仅接口预注册返回 `CodeUnimplemented`，不臆测格式。

- **（更新）** 在 `cmd/polaris/cli_eval_bench.go` 中提供 `--execute` 标志。当不提供时，仅作格式转换验证并提前退出；当提供时，将在 `main.go` 中拦截进入完整的 `bootSubstrate/bootAgent` 启动序列，获取合法的 `RunnerImpl` 并在隔离环境中落盘、执行这批 `EvalCase`，保证底层不绕过安全/评测门控策略。

> 2026-08-09 追记：重新评估触发条件——Terminal-Bench 适配器若要从 `CodeUnimplemented`
> 推进到实现，须先有明确的外部数据格式定义；不得为"接口完整性"臆测格式抢先实现。
