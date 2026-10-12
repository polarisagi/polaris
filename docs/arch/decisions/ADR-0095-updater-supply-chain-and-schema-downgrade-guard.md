# ADR-0095: 自动更新供应链信任模型与 Schema 降级门控

> 状态：Accepted（决策一/三/四已执行 2026-08-10；决策二已执行 2026-08-11；公钥已内嵌 `release-2026.pem`，客户端 fail-closed，流水线私钥 Secret 是否就位以 `make release-signing-status` 与 release 流水线为准） ｜ 首次：2026-08-10 ｜ 最近复核：2026-10-12 ｜ 模块：`internal/sysmgr/updater/`、`internal/store/`、`.github/workflows/release.yml`

## 决策一：校验值必须有独立信任锚点，二者皆无则拒装

信任锚点必须独立于"从哪里下载"：归档可来自任何镜像，需独立锚定的是**校验值本身**。校验值也取自镜像时，归档与校验值被同一方替换则 SHA-256 必然通过，"校验"退化为自洽性检查。`anchorChecksumTrust` 要求至少一个锚点：

| 锚点 | 强度 | 说明 |
|---|---|---|
| A 发布签名 | 强 | `.sha256.sig` 经内嵌公钥验签；独立于传输路径，校验值取自任何镜像都安全 |
| B GitHub 直连 TLS | 弱但可用 | 校验值取自 `CandidateURLs` 首元素（原始 github.com URL），不受镜像运营方控制 |

2026-08-11 由"Warn + 放行"收紧为拒装：polaris 装完会自我替换并重启，且持有 LLM 凭据与工具执行能力；「留了日志」不构成放行理由。
- 代价边界：GitHub 不可达（无代理的大陆网络）**且**签名未开通时自动更新被拒，需手动下载——刻意取舍；开通签名后该路径恢复可用且更安全
- 禁止在注释里声称校验值以 GitHub 为权威而代码实际允许走镜像（原注释即如此，信任锚点早已可整体落到镜像）；注释与代码一致性无法机械判定，靠本 ADR 锚定
- 门控：`checksum_trust_test.go` 的 `TestAnchorChecksumTrust_{AnchorB,NoAnchorIsRejected,SignatureRescuesMirrorPath,SignatureStrippedIsRejected}`

## 决策二：非对称签名用固定密钥 ECDSA P-256，Go 标准库签名与验签

**签什么**：签每个 `<archive>.sha256`，不签归档；一次签名同时锚住校验值与归档。
**格式**：ECDSA P-256 / SHA-256 / ASN.1 DER；私钥（未加密 PKCS#8）仅存 GitHub Secrets `POLARIS_RELEASE_PRIVATE_KEY`，公钥集内嵌于 `internal/sysmgr/updater/releasekeys/*.pem`。

已驳方案：
- keyless（Fulcio/Rekor，`sigstore-go` v1.3.0）— 2026-08-10 实测新增 368 个传递模块、验签程序 16.6MB（stdlib 方案 0 模块、2.6MB），与 Tier-0 精简依赖冲突
- 流水线用 `cosign sign-blob --output-signature` / `openssl dgst -sign` — 2026-08-11 实测不通：cosign v3 废弃 `--output-signature`（要求 `--bundle`）且验签期望 P1363 而非 DER；openssl 加密私钥的 `-passin` 在部分 provider 下被忽略。改由 `tools/release_sign.go` 签名，格式由 `TestSignVerifyRoundTrip` 锁死（签名与验签两侧隔一次发布才碰面）；用户仍可用纯 openssl 以公钥独立核验
- 重评触发条件：Go 生态出现不拖 100+ 传递依赖的 sigstore 验证库，或项目已引入同量级依赖

### 开通状态是四象限（`updater.ResolveSigningState`）

| 公钥 | 私钥 | 状态 | 流水线 | 客户端 |
|---|---|---|---|---|
| 无 | 无 | `disabled` | 跳过签名 + `::warning::` | 退回锚点 B |
| 无 | 有 | `forward` | 签名（暂无人验证） | 同上 |
| 有 | 有 | `enforced` | 签名 + 对照**已提交公钥**自验 | fail-closed |
| 有 | 无 | `broken` | **中止发布** | fail-closed（拒装该包） |

`broken` 是唯一致命组合：客户端内嵌公钥即 fail-closed，Secret 被删/过期/轮换时序错误会让发出的 release 被所有已升级客户端拒装而流水线全绿。判定逻辑必须在 Go 里（`release.yml` 经 `tools/release_signing_gate.go` 调用），保证流水线与客户端读同一信任根、跑同一函数，不漂移。门控：`TestResolveSigningState` 穷举四象限、`TestSigningBrokenExplainIsActionable`（文案须含根因、后果与两条处置路径，防"删公钥让 CI 过"）。

### 其他约束

- **自验对照已提交公钥**，不是从私钥导出的公钥（后者恒成立）；语义同客户端 `verifyWithKeys`：**任一**已提交公钥验过即可（轮换期两把公钥一把私钥）
- **中间态可见**：`make release-signing-status`（并入 `check-all`，恒不 fail）每次打印开通状态，防"签名尚未开通"悄悄成为永久状态（ADR-0091）
- **签名剥离必须拒绝**：签名已开通却取不到 `.sig` 时拒装，不得回退纯 checksum（`TestAnchorChecksumTrust_SignatureStrippedIsRejected`）。横向适用于全仓验签点；2026-08-11 复查仅发现 `internal/eval/founding_anchor.go` `VerifySignature` 原 `Signature=="" || pubKey==nil → true`，已收紧为"配了公钥就必须有签名"（`founding_anchor_test.go`）
- **轮换：新旧公钥并存**：公钥内嵌二进制，只发新公钥会让老客户端无法验新 release。顺序必须是「先提交新公钥 → 再换 Secret → 发 release → 多数客户端升级后删旧公钥」，反序落入 `broken`。**停用签名不能直接删 Secret**：须先移除公钥并发过渡版本。`TestVerifyReleaseSignature/轮换期新旧公钥并存_两者均通过`
- 运维：手册 `internal/sysmgr/updater/releasekeys/README.md`；`scripts/release-signing.sh`（`init`/`rotate`/`status`/`verify`/`retire`，`rotate` 用 git 状态强制「先推公钥、后换 Secret」，`retire` 不代删 Secret）；`polaris release-key show|verify` 用与 updater 相同的内嵌公钥离线验手工下载产物，刻意不提供 `genkey`/`sign`；密钥不随发版轮换，仅泄漏/卫生轮换/算法迁移才 rotate

## 决策三：库 schema 版本高于二进制时 fail-closed

`SQLiteStore.runMigrations` 前调用 `guardSchemaDowngrade`（`internal/store/store.go`）：库中 `schema_versions` 最高版本超过本二进制内嵌 `internal/protocol/schema/*.sql` 最高版本时拒绝启动并给指引。只比最高版本号即可：迁移编号单调递增不复用（`decisions/README.md` 三项不可变），"库有未知版本号"与"库比二进制新"等价。
- 迁移系统只向前，对"库比我新"无感；回滚/共享数据目录/手工换旧包会让旧二进制在新 schema 上报出指向某条 SQL 的误导错误，且可能已写入脏数据
- 代价边界：多付一次启动失败，换掉静默数据腐败（可见可逆 vs 不可见不可逆）
- 门控：`internal/store/schema_downgrade_guard_test.go` `TestGuardSchemaDowngrade`

## 决策四：updater 不做 schema 兼容性预检

updater 拿不到目标版本 schema 清单（需新增 release 侧 manifest），且决策三启动期门控覆盖同一风险面，对不经 updater 的路径（手工换二进制、共享数据目录）同样有效。已驳：updater 拉取 release tag 的 schema manifest 确认可迁移后放行——检查点在可被绕过的路径上且引入新工件。

## 反例守护

- 不得因"checksum 已校验"认为供应链问题闭环——校验值的来源才是问题
- 不得"取不到 .sig 就跳过"
- 不得先换 Secret 后提交公钥；不得为让 CI 过而删公钥

## 重新评估触发条件

- 决策二：Go 生态出现不拖 100+ 传递依赖的 sigstore 验证库，或项目已引入同量级依赖

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-08-10 | 决策一（初为 Warn + 放行）、三、四 |
| 2026-08-11 | 决策一收紧为拒装；决策二落地（固定密钥、四象限、自验、签名剥离、founding_anchor 收紧、轮换规则） |
| 2026-09-21 | 与 ADR-0096 决策四/八对齐：桌面版 `~/.polaris/bin/` 就地替换与 tarball 行为一致，安装包完整性同由本 ADR 承接 |
| 2026-10 | 公钥 `release-2026.pem` 已提交（客户端 fail-closed） |

## 引用代码

`internal/sysmgr/updater/`（`signature.go`、`signer.go`、`signing_state.go`、`releasekeys/`）、`tools/release_sign.go`、`tools/release_signing_gate.go`、`scripts/release-signing.sh`、`cmd/polaris/cli_release_key.go`、`.github/workflows/release.yml`、`internal/store/store.go`、`internal/eval/founding_anchor.go`
