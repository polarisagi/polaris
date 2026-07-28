# ADR-0081: 架构文档结构治理 —— 拆分提案暂缓 + 失效路径引用 CI 门控

- **状态**: Accepted（门控部分已实施）/ Deferred（四条拆分提案）
- **日期**: 2026-07-28
- **决策者**: MrLaoLiAI
- **相关模块**: `docs/arch/` 全体 / `scripts/docs-refs.sh` / `Makefile` / CI

## 上下文

2026-07-28 完成一轮 10 批次架构文档全量审计（产出在 `local_playground/reports/arch-audit/`），共 74 条发现 + 5 条结构性建议（DS-10-001~005）。文档侧缺陷已全部修复，遗留两类需要决策的问题：

1. **五条结构性建议**：拆分 M07（65.8KB）、拆分 M13（65.3KB）、新建 M14-Execute-Engine、M05/M10 检索职责去重、补齐 bootstrap 启动关停专题。
2. **根因未治理**：本轮机械预扫发现 69 处失效路径引用（文档写的代码路径在仓库里不存在），成因是代码侧包拆分/文件迁移后文档未跟进。修完这 69 处不解决任何问题——上一轮审计（ADR-0061/0062）之后同类漂移照样复发，因为没有任何机制在 PR 阶段拦住它。

## 决策

### 一、建立 `make docs-refs` 失效路径引用门控（本轮实施）

照 ADR-0062 建立 `make deadcode` 的同一思路：**这类缺陷 100% 机械可检，就不该靠人工全量审计周期性打捞**。

- `scripts/docs-refs.sh` + `scripts/docs-refs-allowlist.txt`
- 扫描范围：`docs/arch/*.md`（活文档）+ 根 `CLAUDE.md` + `internal/*/CLAUDE.md`
- 已接入 `make check-all`、`.github/workflows/ci.yml` docs-toc job、`scripts/ci_test.sh` 第 4 步

两条刻意的排除规则：

1. **不扫 `docs/arch/decisions/`**。ADR 按定义记录**写作当时**的代码事实。事后把 ADR 正文里的旧路径改成新路径，等于篡改历史记录，会让"当初为什么这么决策"失去可追溯上下文。ADR 里的路径漂移是预期状态，不是缺陷。本轮全量扫描显示 ADR 目录有 ~50 处此类引用，若纳入门控只会逼出一份毫无信息量的巨型白名单。
2. **带点且扩展名不在已知集合的 basename 视为 Go 符号点记法**（`pkg/concurrent.SafeGo`、`internal/config.SandboxConfig`），不报。误报淹没真实缺陷比漏报更致命——一个天天 FAIL 的门控等于没有门控。

白名单只收一类：活文档正在**记载历史**（"该路径已于某日删除/迁移至 X，见 ADR-XXXX"），路径不存在恰恰证明文档是对的。当前 8 条，每条须注明"为何该路径注定不存在"。

### 二、DS-10-005 补齐启动与关停协议（本轮实施）

`ARCHITECTURE.md §8` 新增，同时记录了两件事：生产实际走 `cmd/polaris/boot_*.go` 六阶段手工装配链；`internal/bootstrap/` 那套 `Bootable` + Kahn 拓扑排序 + 四阶关停契约**已定义但零 import、从未在生产路径执行**。

顺带订正一处长期存在的表述冲突：`Module-Dependency-Axioms.md §2.4` 的 [MUST]「注入必须且仅能在 `bootstrap` 中完成」描述的是目标契约而非代码事实，已改为约束"装配层"这一职责概念，并注明当前物理落点。

### 三、DS-10-001 / 002 / 003 / 004 暂缓（Deferred）

四条均为纯文档结构重构，**本轮不实施**。理由与 ADR-0044（M7 模块边界拆分暂缓）同构：

1. **收益停留在理论层面**。四条提案援引的问题是"体量大、加载费上下文、职责混杂"，但没有一条附带真实损害记录——没有一次"因为 M07 太大导致上下文溢出、任务失败"的可复现案例。而 `docs/arch/INDEX.md §2 场景加载表` + 每个 M_X 文件头的 `<!-- §跳读 -->` 行号索引（由 `make docs-sync` 自动维护、`make docs-check` 门控）已经提供了按需局部读取的能力——这正是拆分想解决的问题的现有解法。
2. **代价确定且不小**。`M07` 被外部引用 72 处、`M13` 42 处（含 `docs/`、`internal/*/CLAUDE.md`、代码注释）。拆分需同步改：INDEX.md §1 文档清单与 §2 场景表、根 CLAUDE.md 文档加载协议、`tools/sync_doc_toc.go` 的 §跳读维护范围、以及全部跨文档 `M07 §X` 章节号引用。章节号一旦重排，所有历史 ADR 里的 `M07 §4.3` 全部指错位置，且这些 ADR 按上文第一条原则不该改。
3. **DS-10-003（新建 M14-Execute-Engine.md）的前提已被本轮修复消解**。该提案的论据是"`internal/execute` 已是独立模块但文档无落点，设计散落 M04/M08 且存在大量过时旧包引用"。其中"过时旧包引用"本轮已全部修正；"无文档落点"则不成立——`internal/execute/CLAUDE.md`（4.5KB）已是该模块的权威入口，含子包职责划分与"为何是两个子包而非合并"的完整论证，且 Claude Code 进入该目录时自动注入。再造一份 M14 只会制造第三处需要同步的副本，与本 ADR 第一条要治理的漂移根因同源。
4. **文档结构是契约，改它属于架构变更**。根 `CLAUDE.md §文档加载协议` 明确了哪些文件在什么场景加载，拆分会直接改变这份契约。项目宪法禁止"顺手重构未损坏内容"，此类变更应经 ADR 立项而非在缺陷修复批次里夹带。

## 后果

- 正向：失效路径引用从此在 PR 阶段被拦截，不再依赖全量审计周期性打捞；启动/关停序有了架构落点；`bootstrap` 未接线这一事实从"文档与代码静默矛盾"转为"显式记录的已知状态"。
- 负向：M07/M13 继续超 60KB 治理线；M05 与 M10 的检索叙述继续有约 15 行重复；`internal/execute` 的设计权威源在 `internal/execute/CLAUDE.md` 而非 `docs/arch/M_X` 序列里，对只看 `docs/arch/INDEX.md` 的读者不够直观（缓解：INDEX 已有指向）。
- `docs-refs` 门控本身有维护成本：包重构时若忘记同步文档，CI 会红——这正是预期行为，不是缺陷。

## 重新评估触发条件

DS-10-001~004 若被重提，需先给出以下量化证据之一，而不是理论推演：

- 一次真实的上下文溢出/任务失败记录，且根因确认为单个 M_X 文档体量（而非一次加载了多个 M_X）。
- 一次因 M05/M10 检索描述不一致导致的实现错误（两处描述冲突且开发者按错的那份写了代码）。
- `internal/execute` 出现 `internal/execute/CLAUDE.md` 容纳不下的设计增量（如新增第三个子包、或跨 dag/orchestrator 的统一调度语义），使模块级规范文件不再足以承载。
- 文档体量继续增长到 §跳读局部读取也失效（单个 §章节本身超 15KB）。

满足后应作为独立批次推进，不与缺陷修复混批；且拆分方案必须同时给出跨文档章节号引用的迁移方案（历史 ADR 中的章节号引用不得因此指错）。

## 引用代码

- `scripts/docs-refs.sh`、`scripts/docs-refs-allowlist.txt`、`Makefile`（`docs-refs` / `check-all`）
- `.github/workflows/ci.yml`（docs-toc job）、`scripts/ci_test.sh`（第 4 步）
- `docs/arch/ARCHITECTURE.md §8`、`docs/arch/Module-Dependency-Axioms.md §2.4`
- `internal/bootstrap/`（bootable.go / bootstrapper.go，本 ADR 记录其未接线状态）
- `cmd/polaris/main.go` `run()` + `cmd/polaris/boot_*.go`（生产装配链）
- `local_playground/reports/arch-audit/doc-structure-proposal.md`（DS-10-001~005 原始提案）

## 相关 ADR

- ADR-0044：M7 模块边界拆分暂缓 —— 本 ADR 第三部分沿用其"理论收益 vs 确定代价"判据与"重新评估触发条件"格式
- ADR-0062：`make deadcode` 门控 + 白名单 —— 本 ADR 第一部分沿用其门控形态
- ADR-0046：`internal/execute` 模块化 —— DS-10-003 的上游依据
- ADR-0050：SwarmRouter/TopologyEvolver 整包删除 —— 白名单中 `internal/swarm/topology/` 条目的缘由
