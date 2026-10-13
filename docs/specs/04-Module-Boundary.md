# 04 模块边界与依赖方向

> 违反层依赖的修改一律不可接受，除非 `docs/arch/` 有明确例外声明。

## B1 依赖方向（不可逆）

业务代码统一在 `internal/`，按功能域分四层；`pkg/` 仅放无业务逻辑的通用包（`apperr/concurrent/graph/offpeak/types/util/version`），任意层可引用。门控：`Test_inv_NoCrossLayerImport`。

```
L3 接口/治理层（可引用 L2/L1/L0）
  gateway/（REST/SSE/OpenAI 兼容）  automation/（调度 + HITL）  eval/  channel/（TG/Discord）  sysmgr/（updater/locale/osutils）
L2 协同/知识层（可引用 L1/L0；不可引用 L3）
  swarm/（Planner/Supervisor/Topology）  learning/（SurpriseIndex/Reflexion/Curriculum）  knowledge/（RAG + GraphRAG + 连接器）  extension/（MCP/Plugin/Skill/Marketplace）
L1 认知/执行层（可引用 L0；不可引用 L2/L3）
  agent/（FSM/Context）  memory/  tool/  prompt/  sandbox/  action/（CodeAct/LAM/Hook）  vfs/
L0 基础设施层（不可引用 L1/L2/L3）
  store/（SQLite + SurrealDB + MutationBus + EventLog）  observability/  security/（taint/policy/token/network/guard）  llm/  ffi/  sysinfo/  downloader/  runtimeinfo/  config/  protocol/（跨模块类型与接口契约，所有层可引用）  lint/
```

- `internal/execute/`（单/多 Agent 执行引擎，ADR-0046）服务 L1+L2：禁止 import `internal/agent` 具体实现（见其 `CLAUDE.md`）。
- 跨层初始化编排契约 `bootstrap` 与 `PhasedStartup` 已删除（ADR-0112 决策八 #1，2026-10-13）；生产装配落点是 `cmd/polaris/boot_*.go`（`ARCHITECTURE.md §8.2`）。

## B2 跨模块通信通道

唯一通道：`internal/protocol/` 的结构化类型。

- 两个 `internal/` 包之间的共享类型、事件、接口必须在 `internal/protocol/` 定义
- 禁止 `swarm` 直接 import `memory` 拿具体类型；允许经 `protocol.Memory` 接口访问
- Repository 接口定义在 `internal/protocol/repo/`，实现在 `internal/store/repo/`

## B3 新包创建清单

创建 `internal/xxx/` 前自检：属于哪一层（L0-L3）？依赖哪些包、是否违反方向？需暴露哪些接口、是否要加到 `internal/protocol/interfaces_*.go`？状态需落盘吗（HE-6）？需 EventLog 吗（HE-1）？Tier 0（8GB）能跑吗，FeatureGate 在哪？

## B4 Rust 与 Go 的边界

- `rust/substrate/` 不可引用 `internal/` 的任何 Go 包；Rust 职责限于策略评估、向量运算、Wasm 辅助
- Rust 变更同步更新 `internal/protocol/ffi-abi.md`

## B5 契约版本化与破坏性变更

`internal/protocol/` 变更分两类。

### B5.1 加法变更（无需协调）

新增类型、const、字段（zero-value 兼容）、接口方法默认实现。直接提交，commit message 加 `[proto+]`。

### B5.2 破坏性变更（强制流程）

定义：删除/重命名导出符号；改接口方法签名；改 struct 字段语义（同名异义）；改被持久化或写入 `events` 表的常量值；改 protobuf 字段编号。

必须：① 独立 commit，message 加 `[proto-break]`；② 同 PR 同步更新所有 producer/consumer；③ 写 ADR 说明动机；④ `docs/specs/CHANGELOG.md` 追加条目；⑤ 涉及持久化字段，同步迁移 SQL + 回滚脚本。

### B5.3 跨语言边界（FFI）

`extern "C"` 签名变更视同破坏性变更：① 同步 `internal/protocol/ffi-abi.md`；② ABI 版本号递增（`major.minor`，破坏性升 `major`）；③ Go 侧 `purego` 加载时校验版本，不匹配 panic（禁与 CGO 混用，ADR-0011）；④ 写 ADR 说明 ABI 兼容策略。

### B5.4 用户感知边界

`internal/gateway/` 暴露的 HTTP / gRPC / CLI 契约变更视同 B5.2；版本化经路径前缀 `/v1/...` 或 gRPC service 版本号。
