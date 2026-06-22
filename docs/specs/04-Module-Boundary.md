# 04 模块边界与依赖方向

> 所有违反层依赖的修改都不可被接受，除非架构文档（docs/arch/）中有明确例外声明。

## B1 依赖方向（不可逆）

所有业务代码现统一在 `internal/` 下，按功能域分四层。`pkg/` 仅保留三个无业务逻辑的通用工具包（`apperr` / `types` / `version`），可被任意层引用。

```
L3 接口/治理层 → L3 可引用 L2, L1, L0
  internal/gateway/    HTTP API 网关（REST/SSE/OpenAI 兼容）
  internal/automation/ 定时调度与 HITL 工作流
  internal/eval/       评估与 Benchmark 引擎
  internal/channel/    聊天平台双向适配器（TG/Discord）
  internal/sysmgr/     系统资源管理（下载/更新）
  internal/cli/        命令行引导
    ↑
L2 协同/知识层 → L2 可引用 L1, L0；不可引用 L3
  internal/swarm/      多 Agent 协同（Orchestrator/Planner/Supervisor/Topology）
  internal/learning/   自进化引擎（SurpriseIndex/Reflexion/Curriculum）
  internal/knowledge/  RAG + GraphRAG + 知识连接器
  internal/extension/  扩展注册（MCP/Plugin/Skill/Marketplace）
    ↑
L1 认知/执行层 → L1 可引用 L0；不可引用 L2/L3
  internal/agent/      Agent 核心状态机（FSM/DAG/Context）
  internal/memory/     四层记忆体系（Working/Episodic/Semantic/Procedural）
  internal/tool/       工具注册与执行
  internal/prompt/     提示词模板管理
  internal/sandbox/    沙箱执行环境（Wasm/容器）
  internal/action/     动作层（CodeAct/LAM/Hook）
  internal/vfs/        虚拟文件系统隔离
    ↑
L0 基础设施层 → L0 不可引用 L1/L2/L3
  internal/store/      存储（SQLite + SurrealDB + MutationBus + EventLog）
  internal/observability/ 可观测（metrics/probe/trace）
  internal/security/   安全（taint/policy/token/network/guard）
  internal/llm/        大模型对接层（adapter/stt/tts/router）
  internal/ffi/        Rust dylib 桥接（purego）
  internal/config/     配置加载
  internal/protocol/   跨模块共享类型与接口契约（所有层均可引用）
  internal/errors/     统一错误类型（所有层均可引用）
  internal/lint/       CI 静态扫描规则
```

## B2 跨模块通信通道

唯一通道：`internal/protocol/` 中的结构化类型。

- 任何两个 `internal/` 包之间的共享类型、事件、接口，必须在 `internal/protocol/` 定义
- 禁止：`internal/swarm/` 直接 import `internal/memory/` 拿具体类型
- 允许：`internal/swarm/` 通过 `protocol.Memory` 接口访问记忆
- Repository 接口定义在 `internal/protocol/repo/`，对应实现在 `internal/store/repo/`

## B3 新包创建清单

创建一个新的 `internal/xxx/` 包前，自检以下条目：

- [ ] 这个包属于哪个层？（L0-L3）
- [ ] 它依赖哪些现有包？是否违反依赖方向？
- [ ] 它需要暴露哪些接口给上层？是否需要在 `internal/protocol/interfaces.go` 添加？
- [ ] 它的状态需要落盘吗？（HE-Rule-6 检查）
- [ ] 它需要 EventLog 记录吗？（HE-Rule-1 检查）
- [ ] Tier 0（8GB）能正常运行吗？如果不能，FeatureGate 在哪？

## B4 Rust 与 Go 的边界

- `rust/substrate/` 不可引用 `internal/` 的任何 Go 包
- Rust 职责限于：策略评估、向量运算、Wasm 辅助
- Rust 的变更必须同步更新 `internal/protocol/ffi-abi.md`

## B5 契约版本化与破坏性变更

`internal/protocol/` 是跨模块共享类型的唯一通道（B2）。其变更分两类。

### B5.1 加法变更（无需协调）

- 新增类型、新增 const、新增字段（带 zero-value 兼容）、新增接口方法默认实现
- 直接提交，commit message 加 `[proto+]` tag

### B5.2 破坏性变更（强制流程）

破坏性变更定义：

- 删除/重命名导出符号
- 修改接口方法签名
- 修改 struct 字段语义（同名但解释改变）
- 修改常量值（被持久化或写入 `events` 表的）
- 修改 protobuf 字段编号

破坏性变更必须：

1. 独立 commit，message 加 `[proto-break]` tag
2. 同 PR 内同步更新所有 producer / consumer
3. 写一份 ADR 说明动机（`docs/arch/decisions/`）
4. `internal/protocol/CHANGELOG.md` 追加条目
5. 如涉及持久化字段，同步迁移 SQL + 回滚脚本

### B5.3 跨语言边界（FFI）

`rust/substrate/` 的 `extern "C"` 签名变更视同破坏性变更：

1. 同步更新 `internal/protocol/ffi-abi.md`
2. ABI 版本号递增（`major.minor`，破坏性升 `major`）
3. Go 側 `purego` 加载时校验版本号，不匹配则 panic（禁止与 CGO 混用，见 ADR-0011）
4. 写 ADR 说明 ABI 兼容策略

### B5.4 用户感知边界

`internal/gateway/` 暴露的 HTTP / gRPC / CLI 契约变更视同 B5.2（破坏性变更）。版本化通过路径前缀 `/v1/...` 或 gRPC service 版本号。
