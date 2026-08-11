# Release 签名公钥信任根

本目录内的 `*.pem` 文件是 **发布产物签名的可信公钥集**，随二进制一起编译进程序
（`internal/sysmgr/updater/signature.go` 的 `//go:embed`）。

决策与理由见 `docs/arch/decisions/ADR-0095-updater-supply-chain-and-schema-downgrade-guard.md`。

## 当前状态

**目录内暂无 `.pem` —— 签名尚未开通。**

此状态下 updater 按"信任根未配置"处理：跳过验签、退回纯 checksum 校验，
并在每次更新时打 Warn + 累加 `polaris_updater_signing_not_provisioned_total`。
**这不是静默降级**，是"功能未开通"，且可观测。

## 开通（一次性，需要仓库管理员执行）

```bash
# 1. 生成密钥对（私钥会用你输入的口令加密）
cosign generate-key-pair
#    产出 cosign.key（私钥，加密）+ cosign.pub（公钥，SPKI PEM）

# 2. 私钥与口令存进 GitHub Secrets，本地删除私钥
gh secret set COSIGN_PRIVATE_KEY < cosign.key
gh secret set COSIGN_PASSWORD          # 交互输入第 1 步的口令
shred -u cosign.key                    # macOS: rm -P cosign.key

# 3. 公钥落盘进本目录并提交
cp cosign.pub internal/sysmgr/updater/releasekeys/release-2026.pem
go test ./internal/sysmgr/updater/     # TestEmbeddedTrustStoreParses 校验格式
git add internal/sysmgr/updater/releasekeys/release-2026.pem
```

提交后**下一个 release 起自动签名，客户端自动转为 fail-closed 验签**——
代码侧无需任何改动，信任根一非空就生效。

## 轮换

两条约束，顺序错了都会出事：

1. **新旧公钥并存一段时间再删旧的**——公钥内嵌在二进制里，只发新公钥会让所有
   老版本客户端无法验证新 release（它们的二进制里没有新公钥）。
2. **先提交新公钥，再换 Secret**——反序会让流水线落进 `broken` 判定：
   Secret 已是新私钥，而 `releasekeys/` 里只有旧公钥，签名自验找不到匹配的
   已提交公钥，发布中止。按正确顺序则全程无中断：并存期两把私钥签的都验得过。

```bash
cosign generate-key-pair                                             # ① 生成新密钥对
cp cosign.pub internal/sysmgr/updater/releasekeys/release-2027.pem   # ② 新公钥入库（旧的先别删）
git add internal/sysmgr/updater/releasekeys/release-2027.pem && git commit && git push
gh secret set COSIGN_PRIVATE_KEY < cosign.key                        # ③ 再让 CI 改用新私钥
shred -u cosign.key
# ④ 发一个 release → 等绝大多数客户端升到含新公钥的版本 → 再删 release-2026.pem
```

## 停用

**不要直接删 Secret**：客户端二进制里仍内嵌着公钥、仍在 fail-closed，删 Secret
会让流水线发出不带签名的包，而那些客户端会全部拒绝安装——等于把存量用户锁死在
旧版本。正确做法是先从本目录移除公钥并发一个过渡版本，待客户端升级后再停签名。

`tools/release_signing_gate.go` 会在 release 流水线里拦住这个组合并给出同样的提示。

## 文件命名

`release-<年份>.pem`，一个文件一个公钥。文件名不参与校验逻辑（验签时遍历全部
公钥逐个尝试），仅供人类识别轮换代次。
