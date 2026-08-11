//go:build ignore

// release_sign 对 release 产物的 .sha256 文件批量签名（ADR-0095 决策二）。
//
// 由 .github/workflows/release.yml 的 publish job 调用，替代原先的
// `cosign sign-blob`——cosign v3 已移除可用的分离式签名路径，理由详见
// internal/sysmgr/updater/signer.go 头部。
//
// 用法：
//
//	POLARIS_RELEASE_PRIVATE_KEY="$(cat key.pem)" \
//	  go run tools/release_sign.go artifacts/*.sha256
//
// 对每个入参文件 F 产出 F.sig（base64 的 ASN.1 DER ECDSA 签名）。
//
// 签完立即用**仓库已提交的公钥**（internal/sysmgr/updater/releasekeys/*.pem）
// 自验一遍：只要有任一公钥验得过即通过，与客户端 verifyWithKeys 同语义
// （轮换期新旧公钥并存，私钥只有一把，要求"每把都验过"会让轮换期发布必然失败）。
// 不自验就发布，等于把验证成本推给用户；而用"从私钥导出的公钥"自验则毫无意义
// ——那在数学上恒成立。真正要回答的是"客户端内嵌的公钥验不验得过"。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"
)

func main() {
	files := os.Args[1:]
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "::error::release_sign: 未传入待签名文件")
		os.Exit(2)
	}

	keyPEM := os.Getenv("POLARIS_RELEASE_PRIVATE_KEY")
	if keyPEM == "" {
		fmt.Fprintln(os.Stderr, "::error::release_sign: POLARIS_RELEASE_PRIVATE_KEY 未设置")
		os.Exit(2)
	}

	for _, f := range files {
		payload, err := os.ReadFile(f) //nolint:gosec // 路径来自流水线通配，非用户输入
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::release_sign: 读取 %s 失败: %v\n", f, err)
			os.Exit(1)
		}
		sig, err := updater.SignChecksumFile([]byte(keyPEM), payload)
		if err != nil {
			// 不打印 err 之外的任何东西——私钥内容绝不能进日志。
			fmt.Fprintf(os.Stderr, "::error::release_sign: 签名 %s 失败: %v\n", f, err)
			os.Exit(1)
		}
		if err := os.WriteFile(f+".sig", []byte(sig), 0o644); err != nil { //nolint:gosec // 签名是公开数据
			fmt.Fprintf(os.Stderr, "::error::release_sign: 写入 %s.sig 失败: %v\n", f, err)
			os.Exit(1)
		}
		fmt.Printf("signed: %s -> %s.sig\n", filepath.Base(f), filepath.Base(f))

		// 自验：对照已提交公钥，而非从私钥导出的公钥。
		if err := updater.VerifyReleaseSignature(payload, sig); err != nil {
			fmt.Fprintf(os.Stderr,
				"::error title=签名自验失败::%s 的签名无法被 releasekeys/ 中任何一把已提交公钥验证。"+
					"客户端内嵌的正是这些公钥，此包一旦发出，所有已升级客户端都将拒绝安装。"+
					"最常见原因：Secret 里的私钥已轮换但新公钥尚未提交进 releasekeys/。原始错误: %v\n", f, err)
			os.Exit(1)
		}
		fmt.Printf("verified against committed trust store: %s\n", filepath.Base(f))
	}

	fmt.Printf("全部 %d 个 .sha256 已签名并对照已提交公钥自验通过\n", len(files))
}
