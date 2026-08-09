# ADR-0082: MemFS —— 扩展 core_memory_edit 实现显式可编程记忆块

- **状态**: Accepted（已执行）
- **日期**: 2026-08-02
- **决策者**: 系统架构师
- **相关模块**: M5 `internal/memory/store/core_memory.go`、`internal/tool/builtin/core_memory_edit*.go`、`internal/protocol/interfaces_memory.go`

## 上下文

`core_memory_edit` 工具已支持 `set`/`append`/`delete`，但 Agent 无法枚举自己有哪些块、无法做精确子串替换（长块编辑必须整块重写）、无块级元信息（大小/污点/更新时间）、无容量可见性、无块级保护机制。Letta 等业界实现将这类能力称为 MemFS（块寻址 + 精确编辑）。

## 决策

以**扩展既有 `core_memory_edit` 工具的 operation 集合**方式实现 MemFS，operation 枚举扩为 `["list", "get", "set", "append", "replace", "delete", "describe"]`，不新建独立工具族。

- `protocol.CoreMemory` 接口新增 `Replace`（块内精确子串替换，`old_str` 必须唯一匹配，除非 `replace_all=true`）与 `Describe`（记录块用途说明）两个方法；`Get`/`Set`/`Delete`/`List` 签名不变。
- `core_memory_blocks` 表（`034_core_memory.sql`）新增三列：`description`（块用途自述）、`read_only`（保护块标记，1 = 拒绝一切写操作）、`max_bytes`（单块字节上限，写入时由 `internal/config` 的 `core.memory_block_max_kb` 阈值取值固化到新建行，不追溯已存在行）。
- 容量策略分层：单块上限走每行 `max_bytes`（可因阈值调整对新块生效，旧块保留创建时的额度契约）；总量上限沿用既有全局阈值 `core.memory_total_max_kb`（不新增重复 SSoT）；新增块数上限 `core.memory_max_blocks`（新阈值，见 state.yaml）。
- 污点 only-up：`replace`/`append` 写入后 `taint_level = max(原值, 入参污点)`，入参污点取 `S_VALIDATE` 注入的授信 `TaintLevel`（`protocol.CtxTaintLevelKey`），不采信 LLM 生成参数。
- `list` 操作返回的 JSON **不含 content**，只含 `key/description/size_bytes/max_bytes/read_only/updated_at`，避免一次 list 把整个记忆区重复灌入上下文。
- 全部 operation 经既有 `sandbox.InProcessFn` → `ExecuteTool` 路径，不新增旁路。

## 后果

- **正向**: Agent 获得块枚举、精确编辑、容量自省能力；无需新增工具占用 `tool_search` 检索空间与 LLM 上下文（工具目录已 40+）。
- **负向**: `read_only` 保护块目前只有强制枚举与写路径拒绝逻辑，尚无系统侧创建保护块的生产调用点（无持久化 persona/安全约束类保护块的实际写入者）——机制先行，产出方留待后续按需接入，登记于 `local_playground/upgrade/99-new-findings.md`。
- **反例守护**: 禁止把 MemFS 做成通用 KV（那是 `memory_write` 的职责边界，二者不得合并）；禁止任何 operation 绕过 `ExecuteTool` 直接写库（HE-7）；禁止 `list` 返回内容（防止 token 放大）；禁止用 `updated_at` 冒充容量/大小信息。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 新建 `memfs_*` 独立工具族（对齐 Letta 5 个独立工具） | 与既有 `core_memory_edit` 语义重叠，徒增工具目录体积与模型选择工具的认知负担；违反"最少代码集" |
| `max_bytes` 全部沿用全局配置动态读取（不落列） | 无法表达"块创建时的容量契约"，配置调整会静默改变已存在块的行为预期，且无法支持未来按块差异化限额 |
| `list` 返回全部 content | 与本工具"减少 token 放大"的设计目标直接冲突 |

## 引用代码

- `internal/protocol/interfaces_memory.go`（`CoreMemory` 接口）
- `internal/memory/store/core_memory.go`（`SQLCoreMemoryStore` 实现）
- `internal/tool/builtin/core_memory_edit.go` / `core_memory_edit_exec.go`（工具 schema 与分发）
- `internal/protocol/schema/034_core_memory.sql`（DDL）
- `docs/arch/spec/state.yaml §m5_memory`（`core_memory_max_blocks` 阈值）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-02 | 初稿，随阶段05 P-01 落地 |
| 2026-08-09 | 追记：重新评估触发条件——若出现真实的系统侧保护块创建需求（persona/安全约束类），先补齐生产调用点而非放任"机制先行、产出方留待后续"长期悬空；`list` 不返回 content 的边界只在证明存在无法通过 `get` 单独获取内容的合理场景时才重议。 |
