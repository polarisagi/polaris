# Release 签名公钥信任根

本目录内的 `*.pem` 是 **发布产物签名的可信公钥集**，随二进制编译进程序
（`internal/sysmgr/updater/signature.go` 的 `//go:embed`）。

决策与理由见 `docs/arch/decisions/ADR-0095-updater-supply-chain-and-schema-downgrade-guard.md`。

## 密码学参数

| 项 | 取值 |
|---|---|
| 算法 | ECDSA P-256 |
| 摘要 | SHA-256 |
| 签名编码 | ASN.1 DER，再 base64（`<archive>.sha256.sig` 的内容） |
| 私钥格式 | 未加密 PKCS#8 或 SEC1 PEM |
| 公钥格式 | SPKI PEM（本目录内的 `*.pem`） |

签名与验签均由 Go 标准库完成（`signer.go` / `signature.go`），**不依赖 cosign 或
openssl**。理由见 `signer.go` 头部：cosign v3 已移除分离式签名路径，openssl 3 的
加密 PKCS#8 解码路径存在 provider 缺陷，二者都不适合作为发布流水线的地基；
而 ECDSA-P256-SHA256/DER 是标准原语，Go 标准库直接支持。

## 当前状态

**目录内暂无 `.pem` —— 签名尚未开通。**

此状态下 updater 的信任锚点退回「GitHub 直连 TLS」：校验值取自 github.com 直连时
放行，只能从镜像取得时**拒绝安装**（无任何可用锚点）。开通签名后镜像路径重新
可用且安全。

## 密钥不需要每次发版轮换

**一把密钥签所有 release。** 生成一次之后，每个 `v*` tag 由流水线自动用同一把
私钥签名，无需任何人工介入。只有下列情形才需要 `rotate`：

- 私钥可能泄漏（误提交、CI 日志泄露、人员交接）
- 长期卫生轮换（数年一次即可）
- 算法迁移

## 一键脚本（推荐）

```bash
scripts/release-signing.sh init                  # 首次开通
scripts/release-signing.sh status                # 查看当前状态
scripts/release-signing.sh rotate                # 轮换（强制「先推公钥后换 Secret」）
scripts/release-signing.sh verify <文件> <签名>   # 离线验签
scripts/release-signing.sh retire                # 停用（含防锁死前置检查）
```

脚本把下面的手工步骤连同前置检查、公私钥配对校验、端到端自检、私钥安全删除
（`trap` 覆盖中断路径）一并做掉。**脚本本身不含任何秘密**——它生成密钥并推进
GitHub Secrets，私钥从不落进仓库，故随仓库入库。

## 手工步骤（脚本的等价展开，供理解与应急）

```bash
# 1. 生成 ECDSA P-256 私钥（未加密 PKCS#8）与公钥。在仓库外的目录做。
mkdir -p ~/polaris-signing && cd ~/polaris-signing
openssl ecparam -genkey -name prime256v1 -noout \
  | openssl pkcs8 -topk8 -nocrypt -out release.key
openssl pkey -in release.key -pubout -out release.pub

# 2. 私钥存进 GitHub Secrets（仓库目录下执行，gh 会自动定位仓库）
cd /path/to/polaris
gh secret set POLARIS_RELEASE_PRIVATE_KEY < ~/polaris-signing/release.key

# 3. 公钥落盘进本目录并提交
cp ~/polaris-signing/release.pub internal/sysmgr/updater/releasekeys/release-2026.pem
go test ./internal/sysmgr/updater/     # 校验公钥格式与签名/验签 round-trip
git add internal/sysmgr/updater/releasekeys/release-2026.pem

# 4. 立即安全删除本地私钥（macOS 用 rm -P，Linux 用 shred -u）
rm -P ~/polaris-signing/release.key && rmdir ~/polaris-signing
```

提交后**下一个 `v*` tag 起自动签名，客户端自动转为 fail-closed 验签**——代码侧
无需任何改动，信任根一非空就生效。

### 私钥为何不加口令

私钥只存在于 GitHub Secrets（静态加密、仅注入工作流进程）。再加一层口令意味着
口令也得存进同一个 Secrets 库——能拿到其一的攻击者基本也拿得到其二，防御增益接近
于零，却换来一整类「CI 读不到口令」的失败模式。口令真正的价值是保护**磁盘上的
密钥副本**，而正确做法是生成后立即删除本地副本（上面第 4 步）。

## 轮换

两条约束，顺序错了都会出事：

1. **新旧公钥并存一段时间再删旧的**——公钥内嵌在二进制里，只发新公钥会让所有
   老版本客户端无法验证新 release（它们的二进制里没有新公钥）。
2. **先提交新公钥，再换 Secret**——反序会让流水线落进 `broken` 判定：
   Secret 已是新私钥，而 `releasekeys/` 里只有旧公钥，签名自验找不到匹配的
   已提交公钥，发布中止。按正确顺序则全程无中断：并存期两把私钥签的都验得过。

```bash
openssl ecparam -genkey -name prime256v1 -noout \
  | openssl pkcs8 -topk8 -nocrypt -out release.key          # ① 新密钥对
openssl pkey -in release.key -pubout -out release.pub
cp release.pub internal/sysmgr/updater/releasekeys/release-2027.pem   # ② 新公钥入库（旧的先别删）
git add internal/sysmgr/updater/releasekeys/release-2027.pem && git commit && git push
gh secret set POLARIS_RELEASE_PRIVATE_KEY < release.key      # ③ 再让 CI 改用新私钥
rm -P release.key
# ④ 发一个 release → 等绝大多数客户端升到含新公钥的版本 → 再删 release-2026.pem
```

## 停用

**不要直接删 Secret**：客户端二进制里仍内嵌着公钥、仍在 fail-closed，删 Secret
会让流水线发出不带签名的包，而那些客户端会全部拒绝安装——等于把存量用户锁死在
旧版本。正确做法是先从本目录移除公钥并发一个过渡版本，待客户端升级后再停签名。

`tools/release_signing_gate.go` 会在 release 流水线里拦住这个组合并给出同样的提示。

## 用户如何独立核验（不装任何专用工具）

```bash
# 方式一：用 polaris 自带命令（与 updater 用同一份内嵌公钥和验签代码）
polaris release-key verify polaris-linux-amd64.tar.gz.sha256 \
                           polaris-linux-amd64.tar.gz.sha256.sig

# 方式二：纯 openssl（验签用的是公钥，不涉及加密私钥，无 provider 缺陷问题）
base64 -d < polaris-linux-amd64.tar.gz.sha256.sig > sig.der
openssl dgst -sha256 -verify release.pub -signature sig.der \
             polaris-linux-amd64.tar.gz.sha256
```

## 文件命名

`release-<年份>.pem`，一个文件一个公钥。文件名不参与校验逻辑（验签时遍历全部
公钥逐个尝试），仅供人类识别轮换代次。
