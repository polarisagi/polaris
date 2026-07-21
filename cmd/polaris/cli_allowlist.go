// polaris allowlist 子命令组：local_only 网络白名单（M11 §5.3）离线签名工具。
//
//	polaris allowlist genkey [--out <目录>]     本地生成 Ed25519 密钥对
//	polaris allowlist sign --key <私钥文件> --file <白名单 toml>
//
// 密钥隔离原则同 cli_eval.go：私钥只应存在于运维本地磁盘。genkey/sign 两个
// 子命令完全离线执行（不连接 POLARIS_SERVER_URL，不发任何网络请求）——白名单
// 签名验证发生在 polaris serve 启动阶段本地读盘校验，不经过 HTTP。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func runAllowlistCmd(args []string) error {
	if len(args) == 0 {
		printAllowlistHelp()
		return nil
	}
	switch args[0] {
	case "genkey":
		return runAllowlistGenKey(args[1:])
	case "sign":
		return runAllowlistSign(args[1:])
	case "help", "-h", "--help":
		printAllowlistHelp()
		return nil
	}
	return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知子命令: polaris allowlist %s", strings.Join(args, " ")))
}

func printAllowlistHelp() {
	fmt.Println("用法: polaris allowlist <子命令>")
	fmt.Println()
	fmt.Println("  genkey [--out <目录>]                          本地生成 local_only 白名单签名密钥对（私钥不上传，仅本机使用）")
	fmt.Println("  sign --key <私钥文件> --file <白名单 toml 文件>   对白名单文件签名，写出 <文件>.sig")
	fmt.Println()
	fmt.Println("白名单文件本身（config/local_only_network_allowlist.toml）由运营手写，格式：")
	fmt.Println(`  [[entry]]`)
	fmt.Println(`  domain = "api.example.com"`)
	fmt.Println(`  port = 443`)
	fmt.Println(`  protocol = "https"`)
	fmt.Println()
	fmt.Println("genkey 生成的公钥需设置到运行 polaris serve 的环境：")
	fmt.Println("  POLARIS_LOCAL_ONLY_ALLOWLIST_PUBKEY=<genkey 输出的公钥 base64>")
	fmt.Println()
	fmt.Println("Tier3 local_only 模式上限 5 条白名单条目，变更需重启 polaris serve 生效。")
}

// ── polaris allowlist genkey ──────────────────────────────────────────────────

func runAllowlistGenKey(args []string) error {
	outDir := "."
	for i, a := range args {
		if (a == "--out" || a == "-o") && i+1 < len(args) {
			outDir = args[i+1]
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "allowlist genkey: 生成密钥失败", err)
	}
	keyPath := filepath.Join(outDir, "local_only_allowlist.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "allowlist genkey: 写入私钥文件失败", err)
	}
	fmt.Printf("%s  私钥已写入 %s（0600 权限）\n", clr(ansiOk, "✓"), keyPath)
	fmt.Println(clr(ansiWarn, "  请妥善保管此文件：切勿提交到版本库。"))
	fmt.Println()
	fmt.Println("公钥（设置到运行 polaris serve 的环境变量 POLARIS_LOCAL_ONLY_ALLOWLIST_PUBKEY）：")
	fmt.Printf("  %s\n", base64.StdEncoding.EncodeToString(pub))
	return nil
}

// ── polaris allowlist sign ────────────────────────────────────────────────────

// runAllowlistSign 对白名单 TOML 文件的原始字节做 Ed25519 签名，写出 <file>.sig
// （base64 编码），供 internal/security/network.ListSignedAllowlistEntries 验证。
// 纯本地文件操作，不发起任何网络请求。
func runAllowlistSign(args []string) error {
	var keyPath, filePath string
	for i, a := range args {
		switch {
		case (a == "--key" || a == "-k") && i+1 < len(args):
			keyPath = args[i+1]
		case (a == "--file" || a == "-f") && i+1 < len(args):
			filePath = args[i+1]
		}
	}
	if keyPath == "" || filePath == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris allowlist sign --key <私钥文件> --file <白名单 toml 文件>")
	}
	keyRaw, err := os.ReadFile(keyPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "allowlist sign: 读取私钥文件失败", err)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyRaw)))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return apperr.New(apperr.CodeInvalidInput, "allowlist sign: 私钥文件格式无效（应为 polaris allowlist genkey 生成的 base64 编码文件）")
	}
	fileRaw, err := os.ReadFile(filePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "allowlist sign: 读取白名单文件失败", err)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(priv), fileRaw)
	sigPath := filePath + ".sig"
	if err := os.WriteFile(sigPath, []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "allowlist sign: 写入签名文件失败", err)
	}
	fmt.Printf("%s  签名已写入 %s\n", clr(ansiOk, "✓"), sigPath)
	fmt.Println(clr(ansiDim, "  重启 polaris serve 后生效（local_only 白名单变更需重启）。"))
	return nil
}
