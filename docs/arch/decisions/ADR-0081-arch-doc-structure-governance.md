# ADR-0081: 架构文档结构治理 — 失效路径引用 CI 门控 + 拆分提案暂缓（合并 ADR-0044）

- **状态**: Accepted（门控部分已实施）/ Deferred（拆分提案）| **日期**: 2026-07-28 | **模块**: `docs/arch/`

## 决策一：`make docs-refs` 失效路径引用门控

比照 [ADR-0062](./ADR-0062-deadcode-final-settlement.md) `make deadcode` 思路：文档引用不存在的代码路径 100% 机械可检，不应靠周期性人工全量审计打捞。`scripts/docs-refs.sh` 扫描 `docs/arch/*.md`（活文档）+ 根 `CLAUDE.md` + `internal/*/CLAUDE.md`，接入 `make check-all` 与 CI。

两条刻意排除：① **不扫 `docs/arch/decisions/`**——ADR 记录写作当时的代码事实，事后改旧路径等于篡改历史，会让"当初为什么这么决策"失去可追溯上下文，ADR 内路径漂移是预期状态。② 带点但扩展名不在已知集合的 basename 视为 Go 符号点记法，不报（误报淹没真实缺陷比漏报更致命）。白名单只收"活文档正在记载历史"一类，每条须注明"为何该路径注定不存在"。

## 决策二：DS-10-005 补齐启动/关停协议

`ARCHITECTURE.md §8` 新增记录：生产实际走 `cmd/polaris/boot_*.go` 六阶段手工装配链；`internal/bootstrap/`（`Bootable`+Kahn 拓扑排序+四阶关停）已定义但零 import、从未在生产路径执行。

## 决策三：DS-10-001~004（M07/M13 拆分、新建 M14、M05/M10 检索去重）暂缓

**沿用原 ADR-0044 的暂缓判据**：收益停留在理论层面（无真实上下文溢出/任务失败案例，且 `INDEX.md §2 场景表` + 每文件 `§跳读` 索引已提供按需局部读取）；代价确定且不小（M07 被外部引用 72 处、M13 42 处，拆分需同步改 INDEX/CLAUDE.md/章节号引用，且历史 ADR 里的章节号引用不得因此指错）；DS-10-003 的前提（`internal/execute` 无文档落点）已不成立——`internal/execute/CLAUDE.md` 是现成权威入口；文档结构是契约，改动需独立 ADR 立项而非缺陷修复批次夹带。

**重新评估触发条件**（需量化证据，非理论推演）：真实上下文溢出/任务失败记录且根因确认为单文档体量；M05/M10 描述不一致导致的实现错误；`internal/execute` 出现单文件容纳不下的设计增量；单个 §章节本身超 15KB。

## 反例守护

不得以"体量大"为由脱离触发条件直接重启拆分；`docs-refs` 白名单只收历史记载类条目，不得用于掩盖真实文档漂移。

## 引用代码

`scripts/docs-refs.sh`、`scripts/docs-refs-allowlist.txt`、`docs/arch/ARCHITECTURE.md §8`
