# ADR-0089：lint 规则扫描根接回 internal/ + 裸 error 判定按来源收窄

- 状态：Accepted（已执行）
- 日期：2026-08-08
- 关联：ADR-0062（deadcode 门控）、ADR-0081（docs-refs 门控）、ADR-0046（internal/execute 迁移）

## 背景

仓库从 `pkg/*` 迁到 `internal/*` 四层布局后，`internal/lint` 有 8 条规则的扫描根
从未同步，长期对着 29 个文件的 `pkg/` 扫描；真实代码的 1189 个文件在 `internal/`
下从未被检查过。CI 恒绿，给出的是**虚假保证**——其中 5 条直接守 HE-2 安全边界
（污点 `.Content()` 审计、SSRF、裸 `net.Dial`、FFI 边界）。

`Test_inv_NoRawDBExecWriteInGateway` 更极端：扫描根 `pkg/gateway` 在迁移后根本不存在，
`filepath.Walk` 对不存在的目录直接返回，该规则等同从未运行。

2026-07-07 那次复核已经发现过同类问题并抽出 `walkGoFilesUnder(t, root, subdir, ...)`
以便「平移到覆盖 internal/」，但包装函数 `walkPkgGoFiles` 的函数体仍硬编码 `"pkg"`，
用它的 5 条规则原样空转至今——**修了症状，留了根**。

同期发现 4 个豁免表（`ffi_boundary` / `sql_db_field` / `raw_http_calls` / `raw_net_dial`）
共 24 条键全部指向重构前的 `pkg/` 路径，无一命中。**豁免失配与规则空转互相掩盖**：
单看任一侧都不报错。

## 决策一：扫描根接回 internal/，并让「只扫 pkg」的语义无法被无声继承

`walkPkgGoFiles` 更名为 `walkRepoGoFiles` 并同时遍历 `internal/` + `pkg/`。改名是决策的
一部分而非顺手美化——旧名字是这次空转的直接载体，保留它等于把同一个陷阱留给下一个人。

三条直接 `filepath.Walk` 的规则改走新增的 `walkDirsErr(root, []string{"internal", "pkg"}, ...)`；
`NoRawDBExecWriteInGateway` 的扫描根从 `pkg/gateway` 改为 `internal/gateway`。

四个豁免表按现址重建。`raw_http_calls` 与 `raw_net_dial` 接回 `internal/` 后零违规，
豁免表直接清空——**豁免表为空是正确状态，不需要为了「看起来在管事」保留条目**。

### 后果边界

- 接回后暴露的真实违规：FFI 边界 74 处全属合法边界（11 个文件各自一个独立 Rust dylib
  绑定目标，已入豁免表）；storage 层外持 `*sql.DB` 3 处、Gateway 裸 DB 写 2 处、
  遗留 DEBUG 打印 1 处，均已修复。
- **重新评估触发条件**：任何包路径迁移后，除 `make docs-refs` 外必须复查
  (1) `internal/lint` 各规则的扫描根字面量；(2) `internal/lint/testdata/*.json`
  豁免表的键是否仍 `os.Stat` 得到。两者任一失配都会让规则静默失效。

## 决策二：裸 error 判定按 err 的来源收窄

`inv_BareErrorReturnRatchet` 原本见 `return err` 即报。扫描根接回后 baseline 积到 106 条。
AST 溯源显示：

| err 来源 | 数量 |
|---|---|
| 本仓调用（同包函数 30 / 方法·接口 74） | 104 |
| 标准库 / 三方调用 | 1 |
| 无法定位 | 1 |

**按旧判定清偿这 106 条是错的**，理由有二：

1. 本仓被调方按 `pkg/apperr` 约定自行包装，调用点再包一层只会得到 `apperr` 套 `apperr`
   的重复链——信息量为零，噪声翻倍。
2. 规则文本要求的 `fmt.Errorf / errors.Wrap` 与 CLAUDE.md `§项目结构`
   「禁裸 `errors.New`/`fmt.Errorf` 泄漏调用链」**直接冲突**。规则自身违宪。

故判定改为：**只在 err 来自标准库/三方调用时报违规**。此时错误链上除库自己的措辞外
没有任何本仓上下文，调用方拿到 `no such file or directory` 无从定位是谁在读哪个文件。

作用域限定在最内层函数体，且若 `err` 是该函数的形参则跳过——闭包里的 `err` 常常是
回调契约自带的（`filepath.WalkFunc`、sql 事务闭包），`return err` 是把错误交还给框架，
不是丢上下文。

### 后果边界

- baseline 由 106 条降为空：收窄后的判定在全仓零违规。唯一一处初判命中
  （`internal/vfs/workspace_manager.go`）经复核是误报——那里的 `err` 是
  `filepath.WalkFunc` 的形参，`return err` 是把错误交还给框架；作用域限定到最内层
  函数体后即自然排除，无需改动业务代码。**债务是清零的，不是被重新接受的**。
- 已验证规则不是空转：临时注入一处 `_, err := os.ReadFile(p); if err != nil { return err }`
  能被稳定捕获，移除后回绿。
- 删除 `cmd/gen_bare_error_baseline`。该工具无任何 Makefile / CI / 文档引用，且持有一份
  与规则并行的判定逻辑副本——正是本 ADR 决策一要根除的漂移形态。更重要的是，一个
  「一键接受当前全部违规」的工具，恰恰是空 baseline 与空转规则能长期共存的原因。
  需要重新建档时，规则自身的失败输出即是条目清单。
- **重新评估触发条件**：若将来 `pkg/apperr` 不再要求被调方自行包装（即错误上下文的
  责任从被调方移到调用方），本决策失效，须恢复全量判定。

## 决策三：失效路径门控扩展到 .go 注释

ADR-0081 的 `make docs-refs` 只覆盖 `docs/*.md` + `CLAUDE.md`。复核发现 `.go` 注释里有
139 处同类失效引用（`docs/arch` 文件重命名后未同步 41 处、四层布局迁移前的 `pkg/*`
残留、协议层接口拆分后仍指 `interfaces.go` 等）。

同一类 100% 机械可检的缺陷，文档侧有门控后不再复发，代码注释侧没有门控就攒到三位数
——这本身就是「门控 vs 人工复审」的对照实验结果。

新增 `tools/comment_refs.go`，由 `scripts/docs-refs.sh` 串联，**共用同一份白名单**
`scripts/docs-refs-allowlist.txt`。拆成 Go 程序而非塞进 shell：Go 注释里的路径没有反引号
定界，需要「前一个字符不是 `/` 或单词字符」这类回溯判断排除 URL 片段与标识符列表尾巴，
BSD grep 无 `-P` 无法可靠表达。

### 后果边界

- 刻意不报四类：URL 片段、Go 符号点记法、白名单条目、非顶层目录开头的相对路径。
- 白名单新增 12 条，全部是「注释在记载历史」的合法引用（旧顶层包名、已删除文件、
  从未存在过的 baseline 幽灵条目、MCP 协议方法名 `tools/list`）。
- **重新评估触发条件**：若误报开始需要频繁往白名单加非历史性条目，说明判定规则
  （而非白名单）需要收紧——白名单膨胀是规则失准的症状，不是解法。
