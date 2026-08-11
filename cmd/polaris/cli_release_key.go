package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// `polaris release-key` —— 发布签名信任根的自检工具（ADR-0095 决策二）。
//
// 存在理由：自动更新走的是 updater 内部逻辑，用户看不见也插不上手。手工下载
// release 包的人（离线部署、审计、镜像站运营）需要一条不依赖 cosign 安装的路径
// 来回答"我手上这个包是不是官方发的"。本命令用的是与 updater 完全相同的内嵌
// 公钥集与验签代码，因此"CLI 说通过"与"自动更新会接受"严格等价。
//
// 刻意不提供 genkey/sign：私钥生成与签名由 `cosign generate-key-pair` 与 release
// 流水线负责（见 internal/sysmgr/updater/releasekeys/README.md）。在这里再实现
// 一套私钥处理逻辑，只会多出一个可能泄漏私钥的代码路径，且与 cosign 的密钥
// 格式存在漂移风险。

func runReleaseKeyCmd(args []string) error {
	if len(args) == 0 {
		printReleaseKeyHelp()
		return nil
	}
	switch args[0] {
	case "show":
		return runReleaseKeyShow()
	case "verify":
		return runReleaseKeyVerify(args[1:])
	case "help", "--help", "-h":
		printReleaseKeyHelp()
		return nil
	default:
		printReleaseKeyHelp()
		return apperr.New(apperr.CodeInvalidInput, "release-key: unknown subcommand "+args[0])
	}
}

func printReleaseKeyHelp() {
	fmt.Print(`polaris release-key — 发布签名信任根自检

  show                          列出内嵌的发布签名公钥及其指纹
  verify <文件> <签名文件>       用内嵌公钥离线验证签名（无需安装 cosign）

示例（校验手工下载的 release 包）：
  sha256sum -c polaris-linux-amd64.tar.gz.sha256
  polaris release-key verify polaris-linux-amd64.tar.gz.sha256 \
                             polaris-linux-amd64.tar.gz.sha256.sig

等价的纯 openssl 写法（需自行获取 release.pub）：
  base64 -d < polaris-linux-amd64.tar.gz.sha256.sig > sig.der
  openssl dgst -sha256 -verify release.pub -signature sig.der \
               polaris-linux-amd64.tar.gz.sha256
`)
}

func runReleaseKeyShow() error {
	fps := updater.TrustStoreFingerprints()
	if len(fps) == 0 {
		fmt.Println("发布签名：未开通（本二进制内嵌信任根为空）")
		fmt.Println()
		fmt.Println("此状态下自动更新的信任锚点退回「GitHub 直连 TLS」：校验值只能从镜像取得时拒绝安装。")
		fmt.Println("开通流程见 internal/sysmgr/updater/releasekeys/README.md")
		return nil
	}
	fmt.Printf("发布签名：已开通，内嵌 %d 个可信公钥\n\n", len(fps))
	for _, fp := range fps {
		fmt.Println("  " + fp)
	}
	fmt.Println()
	fmt.Println("指纹 = SHA-256(SPKI DER) 前 8 字节，可用系统工具交叉核对：")
	fmt.Println("  openssl pkey -pubin -in release.pub -outform DER | shasum -a 256")
	return nil
}

func runReleaseKeyVerify(args []string) error {
	if len(args) != 2 {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris release-key verify <文件> <签名文件>")
	}
	payloadPath, sigPath := args[0], args[1]

	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "读取 "+payloadPath+" 失败", err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "读取 "+sigPath+" 失败", err)
	}

	if !updater.SigningProvisioned() {
		return apperr.New(apperr.CodeInternal,
			"本二进制内嵌信任根为空（发布签名未开通），无法验证。"+
				"见 internal/sysmgr/updater/releasekeys/README.md")
	}
	if err := updater.VerifyReleaseSignature(payload, string(sig)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "签名验证失败："+payloadPath+" 不可信，请勿使用", err)
	}

	sum := sha256.Sum256(payload)
	fmt.Printf("✓ 签名验证通过\n")
	fmt.Printf("  文件:   %s\n", payloadPath)
	fmt.Printf("  SHA256: %s\n", hex.EncodeToString(sum[:]))
	fmt.Printf("  信任根: %s\n", strings.Join(updater.TrustStoreFingerprints(), ", "))
	return nil
}
