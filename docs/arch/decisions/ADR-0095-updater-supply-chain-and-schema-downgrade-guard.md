# ADR-0095: 自动更新供应链信任模型与 Schema 降级门控

- **状态**: Accepted（决策一/三/四已执行 2026-08-10；决策二已执行 2026-08-11，待管理员配置密钥后生效）
- **日期**: 2026-08-10（决策二 2026-08-11 补充实施）| **模块**: `internal/sysmgr/updater/`, `internal/store/`, `internal/observability/metrics/`, `.github/workflows/release.yml`

## 背景

2026-08-10 全局审核复核期间，在自动更新链路上查出两个此前无人记录的缺口：一个是注释与代码背离（有安全后果），一个是升级/回滚场景下的 schema 版本错配。二者同属"自托管场景下用户现场会炸、而开发机上永远复现不出来"的一类。

## 决策一：checksum 降级到镜像必须显式、可观测

`verifyChecksum` 优先直连 GitHub 取 `<archive>.sha256`；仅当直连完全不可达时降级到镜像候选节点。降级路径保留（中国大陆完全无法访问 GitHub 的部署不降级等于无法更新），但**必须**同时满足：

1. 打 `slog.Error` 明示本次更新处于弱信任模式；
2. 累加 `metrics.GlobalUpdaterWeakTrustVerifyTotal`。

**禁止**在注释里声称校验值以 GitHub 为权威而代码实际允许走镜像。

- **反例守护**：本函数原注释写「checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威」，而代码用 `downloader.CandidateURLs`（含代理节点）取校验值。读注释的人会以为供应链信任锚点仍在 GitHub，实际上校验值与归档可能来自同一个被污染的镜像——此时 SHA-256 只证明"下到的和该镜像宣称的一致"，防篡改能力退化为防传输损坏，而没有任何日志或指标暴露这一事实。
- **门控**：注释与代码的一致性无法机械判定，靠本 ADR + 代码内追记锚定。指标非零可在运维侧告警。

## 决策二：非对称签名用 **cosign 固定密钥**，不用 keyless

`sha256` 校验的信任锚点是"取得校验值的那条链路"。彻底方案是发布者签名——校验值自带签名，镜像无论怎么篡改都伪造不出。**已实施**（2026-08-11）。

### 签什么

签每个 `<archive>.sha256`，不签归档本身。客户端本来就要下载 `.sha256`，签名让这个几十字节的文件可信，归档完整性再由其中的 SHA-256 承接。一次签名同时锚住"校验值"与"归档"，且客户端无需把整个归档喂给验签器。

### 为什么不用 keyless（Fulcio/Rekor）

keyless 免去长期私钥管理，代价是客户端离线验证需要 `sigstore-go`。2026-08-10 实测（`/tmp` 内最小可编译程序对照）：

| 方案 | 传递模块数 | 最小验签程序体积 |
|---|---|---|
| `sigstore-go` v1.3.0 | **368** | **16.6 MB** |
| stdlib `crypto/ecdsa` + `crypto/x509` | 0 | 2.6 MB（= Go 运行时基线） |

当前 polaris 是 111 个模块 / 31.4 MB，接入 keyless 后约 479 个模块 / 45 MB。**用"为修供应链问题而新增 368 个未审计的传递依赖"来换"免去一把私钥的管理"，在本项目上是净负收益**——[Tier-0-Limit] 要求 2GB VPS 可运行，依赖表也是刻意精简的（对照 `M06 §2` 技能加载同样明确选择"本地离线签名，非 cosign"）。

固定密钥方案：ECDSA P-256，私钥只存在于 GitHub Secrets（`COSIGN_PRIVATE_KEY` / `COSIGN_PASSWORD`），签名由流水线的 `cosign sign-blob` 完成；公钥集内嵌在二进制（`internal/sysmgr/updater/releasekeys/*.pem`），验证走 Go 标准库，**零新增依赖**。签名格式与 cosign 完全兼容，用户可用 `cosign verify-blob --key cosign.pub` 独立自验，不被本实现绑架。

- **重新评估触发条件**：Go 生态出现一个不拖 100+ 传递依赖的 sigstore 验证库；或本项目已因其他原因引入同等量级依赖，使边际成本归零。

### 两阶段落地与状态对齐

私钥必须由仓库管理员生成（代码无法代劳），故签名能力分两段生效，且**两侧状态严格对齐**：

| 阶段 | 流水线（`release.yml`） | 客户端（`anchorChecksumTrust`） |
|---|---|---|
| 未开通（`releasekeys/` 无 `.pem`、Secrets 未配） | 跳过签名并 `::warning::` | 退回纯 checksum + Warn + `polaris_updater_signing_not_provisioned_total` |
| 已开通 | 签名并**自验一遍**再发布 | **fail-closed**：`.sig` 缺失或验签失败一律拒绝安装 |

流水线在无密钥时**跳过而非失败**：否则本改动一合入就当场卡死发布流程。客户端在信任根为空时放行，否则存量部署一升级就再也更新不了。开通只需管理员把 `cosign.pub` 提交进 `releasekeys/`——代码零改动，信任根一非空即自动转 fail-closed。

### 签名剥离（signature stripping）必须拒绝

签名已开通却取不到 `.sig` 时**拒绝安装**，不得回退到纯 checksum。否则"开通签名"这件事可以被网络侧单方面撤销：中间人只要丢掉 `.sig` 请求，客户端就自动降级回可被镜像伪造的模式，签名等于没做。

- **反例守护**：`internal/sysmgr/updater/checksum_trust_test.go` `TestAnchorChecksumTrust_SignatureStrippedIsRejected`。后续重构若图省事写成"取不到 .sig 就跳过"，该用例立刻红。

### 密钥轮换：新旧公钥必须并存一段时间

`verifyWithKeys` 遍历全部内嵌公钥逐个尝试，任一通过即成立。公钥内嵌在二进制里，**只发新公钥会让所有老版本客户端无法验证新 release**（它们的二进制里没有新公钥）。轮换顺序：新公钥入库 → CI 改用新私钥 → 发一个 release → 等绝大多数客户端升到含新公钥的版本 → 再删旧公钥。

- 操作手册：`internal/sysmgr/updater/releasekeys/README.md`
- **反例守护**：`TestVerifyReleaseSignature/轮换期新旧公钥并存_两者均通过`

### 运维自检入口

`polaris release-key show` 列出内嵌公钥指纹；`polaris release-key verify <文件> <签名>` 用**与 updater 完全相同的**内嵌公钥与验签代码离线校验手工下载的产物，无需安装 cosign。刻意不提供 `genkey`/`sign`：私钥处理留给 `cosign generate-key-pair` 与流水线，在客户端再实现一套只会多出一条可能泄漏私钥的代码路径。

> 不要因为"checksum 已经校验过了"就认为供应链问题已闭环——校验值本身的来源才是问题所在。

## 决策三：库的 schema 版本高于二进制时 fail-closed

`SQLiteStore.runMigrations` 在应用迁移前调用 `guardSchemaDowngrade`：库中 `schema_versions` 的最高版本超过本二进制内嵌 `internal/protocol/schema/*.sql` 的最高版本时，拒绝启动并给出明确指引。

只比较最高版本号即可，不必逐个比对：迁移编号单调递增且不复用（`CLAUDE.md §项目结构` DDL 修改策略 + `decisions/README.md` 三项不可变），"库里有本二进制不认识的版本号"与"库比二进制新"是等价命题。

- **反例守护**：迁移系统只向前——它跳过 `applied` 中已有的版本，对"库比我新"完全无感。自动更新回滚、多实例共享数据目录、手工换回旧包都会造成旧二进制在新 schema 上运行：撞见新增的 NOT NULL 列、被拆分的表、变更的约束时才报错，错误信息指向某条具体 SQL，完全看不出根因是版本错配，且此刻可能已写入若干条不符合新约束的脏数据。
- **代价边界**：多付一次启动失败，换掉一类静默数据腐败。这是刻意的取舍——启动失败可见且可逆，数据腐败不可见且不可逆。
- **门控**：`internal/store/schema_downgrade_guard_test.go` `TestGuardSchemaDowngrade`。

## 决策四：updater 不负责 schema 兼容性判断

自动更新流程**不**在下载/安装阶段做 schema 预检。理由：updater 拿不到目标版本的 schema 清单（要为此新增一套 release 侧 manifest 与分发），而决策三的启动期门控已经覆盖同一风险面，且覆盖得更全——它对"手工换二进制""共享数据目录"这些不经过 updater 的路径同样有效。

- **反例守护**：审核建议"updater 拉取对应 release tag 的 schema manifest，确认可迁移后才放行覆盖"。该方案把检查点放在了一条能被绕过的路径上，且引入 release 侧新工件。启动期门控是更靠后、更收口的位置。
