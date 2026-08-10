# ADR-0095: 自动更新供应链信任模型与 Schema 降级门控

- **状态**: Accepted | **日期**: 2026-08-10 | **模块**: `internal/sysmgr/updater/`, `internal/store/`, `internal/observability/metrics/`

## 背景

2026-08-10 全局审核复核期间，在自动更新链路上查出两个此前无人记录的缺口：一个是注释与代码背离（有安全后果），一个是升级/回滚场景下的 schema 版本错配。二者同属"自托管场景下用户现场会炸、而开发机上永远复现不出来"的一类。

## 决策一：checksum 降级到镜像必须显式、可观测

`verifyChecksum` 优先直连 GitHub 取 `<archive>.sha256`；仅当直连完全不可达时降级到镜像候选节点。降级路径保留（中国大陆完全无法访问 GitHub 的部署不降级等于无法更新），但**必须**同时满足：

1. 打 `slog.Error` 明示本次更新处于弱信任模式；
2. 累加 `metrics.GlobalUpdaterWeakTrustVerifyTotal`。

**禁止**在注释里声称校验值以 GitHub 为权威而代码实际允许走镜像。

- **反例守护**：本函数原注释写「checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威」，而代码用 `downloader.CandidateURLs`（含代理节点）取校验值。读注释的人会以为供应链信任锚点仍在 GitHub，实际上校验值与归档可能来自同一个被污染的镜像——此时 SHA-256 只证明"下到的和该镜像宣称的一致"，防篡改能力退化为防传输损坏，而没有任何日志或指标暴露这一事实。
- **门控**：注释与代码的一致性无法机械判定，靠本 ADR + 代码内追记锚定。指标非零可在运维侧告警。

## 决策二：非对称签名是目标态，checksum 不是终点

`sha256` 校验的信任锚点是"取得校验值的那条链路"。彻底方案是 sigstore/cosign 无钥签名——校验值自带发布者签名，镜像无论怎么篡改都伪造不出。

**当前不实现**，因为它不是纯代码改动：需要先在 release 流水线接入签名步骤并确定公钥分发方式（硬编码进二进制 / OIDC 身份校验）。在流水线就绪之前落客户端校验，只会让所有更新失败。

**重新评估触发条件**：release 流水线接入 cosign 签名后立即实施；或 `GlobalUpdaterWeakTrustVerifyTotal` 在真实部署中长期非零（说明多数用户实际跑在弱信任模式下，优先级应提升）。

> 不要因为"checksum 已经校验过了"就认为供应链问题已闭环——校验值本身的来源才是问题所在。

## 决策三：库的 schema 版本高于二进制时 fail-closed

`SQLiteStore.runMigrations` 在应用迁移前调用 `guardSchemaDowngrade`：库中 `schema_versions` 的最高版本超过本二进制内嵌 `internal/protocol/schema/*.sql` 的最高版本时，拒绝启动并给出明确指引。

只比较最高版本号即可，不必逐个比对：迁移编号单调递增且不复用（`CLAUDE.md §项目结构` DDL 修改策略 + `decisions/README.md` 三项不可变），"库里有本二进制不认识的版本号"与"库比二进制新"是等价命题。

- **反例守护**：迁移系统只向前——它跳过 `applied` 中已有的版本，对"库比我新"完全无感。自动更新回滚、多实例共享数据目录、手工换回旧包都会造成旧二进制在新 schema 上运行：撞见新增的 NOT NULL 列、被拆分的表、变更的约束时才报错，错误信息指向某条具体 SQL，完全看不出根因是版本错配，且此刻可能已写入若干条不符合新约束的脏数据。
- **代价边界**：多付一次启动失败，换掉一类静默数据腐败。这是刻意的取舍——启动失败可见且可逆，数据腐败不可见且不可逆。
- **门控**：`internal/store/store_test.go` `TestGuardSchemaDowngrade`。

## 决策四：updater 不负责 schema 兼容性判断

自动更新流程**不**在下载/安装阶段做 schema 预检。理由：updater 拿不到目标版本的 schema 清单（要为此新增一套 release 侧 manifest 与分发），而决策三的启动期门控已经覆盖同一风险面，且覆盖得更全——它对"手工换二进制""共享数据目录"这些不经过 updater 的路径同样有效。

- **反例守护**：审核建议"updater 拉取对应 release tag 的 schema manifest，确认可迁移后才放行覆盖"。该方案把检查点放在了一条能被绕过的路径上，且引入 release 侧新工件。启动期门控是更靠后、更收口的位置。
