# ADR-0068: 开放基准适配器架构（Benchmark Adapter）

- **状态**: Accepted（已执行）| **模块**: `internal/eval/harness`

## 决策

引入 `BenchmarkAdapter` 接口（`Name()`/`Load(datasetPath) ([]harness.EvalCase, error)`）将外部开放基准（τ-bench/Terminal-Bench）数据集映射为内部 `EvalCase`，复用既有 `RunnerImpl` 执行引擎，不重新发明执行器。数据集路径由用户运行时指定，不打包进仓库（隐私/许可合规），仓库仅留 2~3 条 fixture。本次仅实现 τ-bench 转换逻辑；Terminal-Bench 因外部格式未定，仅接口预注册返回 `CodeUnimplemented`，不臆测格式。
