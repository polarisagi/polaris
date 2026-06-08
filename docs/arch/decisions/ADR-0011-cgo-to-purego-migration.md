# ADR-0011: cgo → purego 迁移（cedar_ffi.go + surreal_store.go）

- **状态**: Accepted（**已执行完毕** 2026-05-16）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M2 / M11 / `pkg/substrate/policy` / `pkg/substrate/storage` / `rust/substrate`

## 上下文

ADR-0005 决策购零 CGO（purego 桥接），但代码实际滞后：
- `pkg/substrate/policy/cedar_ffi.go` (98 行) 仍用 cgo
- `pkg/substrate/storage/surreal_store.go` (348 行) 仍用 cgo

`00-Global-Dictionary.md §4 §6` 宣称 "purego" 是设计意图，非实施事实。本 ADR 补齐实施计划。

实施滞后产生的具体问题：
1. **交叉编译困难** — cgo 需目标平台 C 工具链
2. **单二进制分发受阻** — Rust dylib 必须随附，`LDFLAGS` 路径硬编码
3. **与 ADR-0003 体系矛盾** — modernc/sqlite 选购零 CGO，FFI 路径却开 CGO

## 决策

**分阶段迁移 `cedar_ffi.go` + `surreal_store.go` 到 purego，引入 ABI 版本协议。**

| Phase | 范围 | 落地代码 |
|-------|------|---------|
| 1 | ABI 版本协议（Rust + Go 双侧 major.minor，不匹配 panic） | `rust/substrate/src/lib.rs` `substrate_abi_version()` |
| 2 | cedar_ffi.go 4 函数迁移（load_policies / evaluate / policy_count / free_string） | `pkg/substrate/policy/cedar_ffi.go` |
| 3 | surreal_store.go 13 函数迁移（KV/Vector/Graph/FTS/free） | `pkg/substrate/storage/surreal_store.go` |
| 4 | Makefile 平台 dylib 拷贝 + 启动加载 | `Makefile` / `pkg/substrate/ffi/dylib.go` |
| 5 | 文档同步（dict §4 §6 / 07 / pkg/substrate AGENTS.md / ffi-abi.md §7） | 见 CHANGELOG 2026-05-16 |

字符串/字节生命周期约定：Go→C 用 null-terminated `[]byte` + `unsafe.Pointer` 转 uintptr；C→Go 立即拷贝立即调 `*_free_*`。

## 后果

- **正向**: ADR-0005 真正落地；零 CGO 交叉编译；与 ADR-0003 一致；ABI 版本协议防 drift
- **负向**: 字符串/字节生命周期手动管理较 cgo 复杂；Windows DLL 加载路径需 GOOS 特化
- **反例守护**:
  - 未来如有人提议"为简化代码改回 cgo" → 本 ADR + ADR-0005 联合拒绝
  - 新增 Rust 库需 Go 调用 → 直接 purego，不可新建 cgo 桥
  - purego 缺特定能力（如 Go callback into Rust）→ 先扩展 purego 或重新设计 ABI，不绕回 cgo

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保持 cgo 现状 | 违反 ADR-0005；交叉编译困难持续；单二进制分发受阻 |
| 一次性全量迁移（不分阶段） | cedar (4) 与 surreal (13) 复杂度差异大，风险叠加 |
| 仅迁 cedar | 不一致性持续；surreal 是更大 FFI 暴露面 |
| gRPC sidecar 隔离 | 增加进程依赖；违反 Tier-0；Cedar 在延迟敏感路径 |
| Rust 重写为纯 Go | Cedar 形式化验证（Lean）不可复现；SurrealDB-Core 重写工作量巨大 |

## 风险与缓解（保留为运行时关注项）

| 风险 | 缓解 |
|------|------|
| ABI 静默 drift | `substrate_abi_version` 启动校验 + `ffi-abi.md` 文档 |
| Use-after-free | "立即拷贝 + 立即归还"模式 |
| Windows DLL 路径 | `bin/lib/libsubstrate.{so,dylib,dll}` GOOS 分发 |
| macOS Gatekeeper | 开发自签；发布走 Apple Developer 流程 |
| 性能回归 | Cedar < 1ms 预算充裕；purego 调用开销 < 100ns |

## 关联 ADR

- [ADR-0005](./ADR-0005-purego-ffi-cedar.md): 设计决策，本 ADR 是实施计划
- [ADR-0003](./ADR-0003-sqlite-modernc-primary-storage.md): 零 CGO 体系一致性论据
- [ADR-0010](./ADR-0010-surrealdb-cognitive-storage.md): SurrealDB-Core 集成

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted；代码执行待 B 阶段批准 |
| 2026-05-16 | Phase 1~5 全部执行完毕；ABI 1.0；`make build/test` 全套绿；副发现 ffi-abi.md §1.1 引擎清单与 lib.rs 不符（已 §7.5 标记） |
