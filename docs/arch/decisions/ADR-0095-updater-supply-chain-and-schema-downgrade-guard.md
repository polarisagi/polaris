# ADR-0095: 自动更新供应链信任模型与 Schema 降级门控

- **状态**: Accepted（决策一/三/四已执行 2026-08-10；决策二已执行 2026-08-11，待管理员配置密钥后生效）
- **日期**: 2026-08-10（决策二 2026-08-11 补充实施）| **模块**: `internal/sysmgr/updater/`, `internal/store/`, `internal/observability/metrics/`, `.github/workflows/release.yml`

## 背景

2026-08-10 全局审核复核期间，在自动更新链路上查出两个此前无人记录的缺口：一个是注释与代码背离（有安全后果），一个是升级/回滚场景下的 schema 版本错配。二者同属"自托管场景下用户现场会炸、而开发机上永远复现不出来"的一类。

## 决策一：checksum 降级到镜像必须显式、可观测

**信任锚点必须独立于"从哪里下载"。** 归档可以来自任何镜像——它的完整性由校验值承接；真正需要独立锚定的是**校验值本身**。一旦校验值也从镜像取得，信任锚点就整体转移给镜像运营方，归档与校验值被同一方替换时 SHA-256 比对必然通过，"校验"退化成"自洽性检查"。

故 `anchorChecksumTrust` 要求校验值由以下两个锚点之一支撑，**二者皆无则拒绝安装**：

| 锚点 | 强度 | 说明 |
|---|---|---|
| **A — 发布签名** | 强 | `.sha256.sig` 经内嵌公钥验签通过。独立于传输路径，校验值取自任何镜像都安全 |
| **B — GitHub 直连 TLS** | 弱但可用 | 校验值取自 `CandidateURLs` 首元素（原始 github.com URL）。信任面含 CA 体系与 GitHub 自身，但不受镜像运营方控制 |

2026-08-11 收紧（此前是 Warn + 放行）：告警放行等于把一个无法证明来源的二进制装进用户机器，而 polaris 装完会自我替换并重启、且持有 LLM 凭据与工具执行能力。**「留了日志」不构成放行理由——没有人在更新成功时读日志。**

- **代价边界**：GitHub 完全不可达（无代理的大陆网络）**且**签名未开通时，自动更新被拒，需手动下载。这不是遗漏而是刻意取舍——该场景下确实无法证明产物来源。开通签名即让这条路径重新可用**且更安全**（锚点 A 不依赖能否直连 GitHub）。
- **禁止**在注释里声称校验值以 GitHub 为权威而代码实际允许走镜像。
- **反例守护**：`checksum_trust_test.go` `TestAnchorChecksumTrust_AnchorB` / `_NoAnchorIsRejected` / `_SignatureRescuesMirrorPath`。原注释写「checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威」，而代码用 `downloader.CandidateURLs`（含代理节点）取校验值——读注释的人会以为信任锚点仍在 GitHub，实际上早已可整体落到镜像上。上述两锚点模型才真正兑现了那句话原本承诺的性质。
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

### 开通状态是四象限，不是开关

私钥必须由仓库管理员生成（代码无法代劳），公钥则随代码提交，二者由不同的人在不同时刻配置——因此"是否已开通"不是一个布尔量，而是 **`releasekeys/*.pem` 是否非空 × 流水线是否持有私钥** 的四象限。判定见 `updater.ResolveSigningState`：

| 公钥 | 私钥 | 状态 | 流水线 | 客户端 |
|---|---|---|---|---|
| 无 | 无 | `disabled` | 跳过签名 + `::warning::` | 退回纯 checksum + Warn + `signing_not_provisioned` 指标 |
| 无 | 有 | `forward` | 签名（此刻无人验证，告警提示） | 同上 |
| 有 | 有 | `enforced` | 签名 + 对照**已提交公钥**自验 | **fail-closed** |
| 有 | 无 | `broken` | **中止发布** | fail-closed（会拒绝这次发布的包） |

`broken` 是唯一的致命组合，也是这套状态机存在的首要理由：客户端一旦内嵌公钥即转 fail-closed（取不到 `.sig` 就拒装，防签名剥离），此时若流水线因 Secret 被删/过期/fork 未配而签不了名，**发出去的 release 会被每一个已升级的客户端拒绝安装，而流水线全绿、无人察觉**——直到用户报"更新一直失败"。轮换时"先提交新公钥、还没更新 Secret"就会落进这一格。

其余三种一律放行：`disabled` 是本特性落地初期的正常态，流水线在无密钥时**跳过而非失败**，否则本改动一合入就当场卡死发布；客户端在信任根为空时放行，否则存量部署一升级就再也更新不了。

**判定逻辑必须在 Go 里，不能写在 YAML 的 shell 里。** 签名是流水线与客户端的双侧协议，两侧对"现在是否该有签名"的判断必须永远一致；各判各的迟早漂移，而漂移的表现是"发出去的包客户端装不上"。现在两侧读同一个信任根、跑同一个 `ResolveSigningState`，`release.yml` 经 `tools/release_signing_gate.go` 调用。

- **反例守护**：`signing_state_test.go` `TestResolveSigningState` 穷举四象限（这张表是协议本身，不是实现细节，故穷举而非抽样）；`TestSigningBrokenExplainIsActionable` 守护致命态文案必须同时含根因、后果与两条处置路径——真触发时通常在半夜发版，第一反应会是"把公钥删了让 CI 过去"，而那恰恰会把存量客户端锁死。

### 自验必须对照已提交的公钥

流水线签完必须自验，且**对照物是 `releasekeys/*.pem`，不是从私钥导出的公钥**。用 `cosign public-key --key env://COSIGN_PRIVATE_KEY` 导出来验只能证明"签名与私钥匹配"——数学上恒成立，等于什么都没验。真正要回答的是"客户端内嵌的那把公钥验不验得过"。两者在轮换场景下分叉：Secret 换了新私钥而 `releasekeys/` 还是旧公钥时，前者照样全绿，后者当场报错——而客户端的遭遇与后者一致。

自验的判定语义同样必须与客户端 `verifyWithKeys` 一致：**任一**已提交公钥验过即可，不是每一把都要验过。轮换期 `releasekeys/` 同时存在新旧两把公钥而私钥只有一把，要求"每把都验过"会让轮换期的每次发布都失败。

### 中间态的可见性

`make release-signing-status`（已并入 `check-all`）每次都会把当前开通状态打出来。目的是让"签名尚未开通"这个中间态被反复看见，而不是悄悄成为永久状态——本仓库已有多次"门控恒绿而问题长期存在"的先例（ADR-0091）。该目标恒不 fail 构建：致命组合只有 CI 判得出，本地无从知晓 Secret 状态。

### 签名剥离（signature stripping）必须拒绝

签名已开通却取不到 `.sig` 时**拒绝安装**，不得回退到纯 checksum。否则"开通签名"这件事可以被网络侧单方面撤销：中间人只要丢掉 `.sig` 请求，客户端就自动降级回可被镜像伪造的模式，签名等于没做。

- **反例守护**：`internal/sysmgr/updater/checksum_trust_test.go` `TestAnchorChecksumTrust_SignatureStrippedIsRejected`。后续重构若图省事写成"取不到 .sig 就跳过"，该用例立刻红。

**该规则横向适用于全仓所有验签点**，不限于 updater。2026-08-11 按此规则复查了全部密钥/签名校验位置（capability_token / local_only allowlist / taint HMAC Unseal / skill Signer / 各 webhook HMAC），只查出一处同类缺陷：`internal/eval/founding_anchor.go` `VerifySignature` 原写作 `Signature == "" || pubKey == nil → true`——攻击者改完 fingerprint 再把 signature 字段删掉即可绕过篡改检测，而该校验的全部目的正是发现文件被改过。已收紧为"配了公钥就必须有签名"，回归防护见 `founding_anchor_test.go`。

### 密钥轮换：新旧公钥必须并存一段时间

`verifyWithKeys` 遍历全部内嵌公钥逐个尝试，任一通过即成立。公钥内嵌在二进制里，**只发新公钥会让所有老版本客户端无法验证新 release**（它们的二进制里没有新公钥）。

**顺序必须是「先提交新公钥 → 再换 Secret」**：新公钥入库（旧的先留着）→ Secret 换新私钥 → 发一个 release → 等绝大多数客户端升到含新公钥的版本 → 再删旧公钥。反序（先换 Secret）会落进 `broken`：Secret 已是新私钥而 `releasekeys/` 只有旧公钥，自验找不到匹配的已提交公钥，发布中止。按正确顺序则全程无中断——并存期两把私钥签的都验得过。

**停用签名同理不能直接删 Secret**：客户端二进制里仍内嵌公钥、仍 fail-closed，删 Secret 会让流水线发出不带签名的包而客户端全部拒装。须先移除公钥并发一个过渡版本。

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
