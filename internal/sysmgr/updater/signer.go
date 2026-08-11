package updater

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// 发布签名的**签名侧**（ADR-0095 决策二）。验签侧见 signature.go。
//
// # 为什么签名也在 Go 里，而不是调 cosign 或 openssl
//
// 2026-08-11 实测推翻了原方案：
//
//   - **cosign v3.1.3 已移除可用的分离式签名路径**。`sign-blob --output-signature`
//     直接报错要求 `--bundle`，`--new-bundle-format` 亦被标记为"未来唯一格式"；
//     `verify-blob --signature` 同样废弃，且期望 IEEE P1363（r‖s 定长）而非 ASN.1
//     DER——连签名的字节格式都换了。cosign v3 正朝 bundle + 透明日志 + keyless
//     演进，而本项目要的恰恰是它在弃用的那条路（离线、分离式、无 Rekor）。
//     跟着它的弃用节奏走，等于给发布流水线绑一颗定时炸弹。
//   - **openssl 3 的加密 PKCS#8 解码路径有 provider 缺陷**：`-passin pass:` /
//     `env:` 均被忽略而去开控制台读口令。同一份脚本在不同 openssl 构建上表现
//     不同，放进 CI 就是不可复现的失败。
//
// 而我们真正需要的只是一个 40 年历史的标准原语：ECDSA-P256 over SHA-256，
// ASN.1 DER 编码。Go 标准库直接支持，跨平台一致、可单测、零外部依赖。
// 用户仍可用 openssl 独立核验（见 releasekeys/README.md），不被本实现绑架。
//
// # 私钥为何不加口令
//
// 私钥只存在于 GitHub Secrets（静态加密、仅注入工作流进程）。再加一层口令意味着
// 口令也得存进同一个 Secrets 库——能拿到其一的攻击者基本也拿得到其二，防御增益
// 接近于零，却换来一整类"CI 读不到口令"的失败模式。口令真正的价值是保护**磁盘上
// 的密钥副本**，而正确做法是生成后立即删除本地副本（见 README）。

// SignChecksumFile 用 PEM 私钥对 payload 签名，返回 base64(ASN.1 DER)。
//
// 输出格式与验签侧 verifyWithKeys 严格对应：先取 payload 的 SHA-256 摘要，
// 再 ECDSA 签名，DER 编码后 base64。两侧格式由 TestSignVerifyRoundTrip 锁死。
func SignChecksumFile(privKeyPEM []byte, payload []byte) (string, error) {
	priv, err := ParseECDSAPrivateKeyPEM(privKeyPEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	der, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "release sign: ecdsa sign", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ParseECDSAPrivateKeyPEM 解析 PEM 私钥，接受 PKCS#8 与 SEC1 两种编码。
//
// 两种都收是因为生成方式不同产出不同：`openssl pkcs8 -topk8 -nocrypt` 给 PKCS#8
// （"PRIVATE KEY"），`openssl ecparam -genkey` 直接给 SEC1（"EC PRIVATE KEY"）。
// 让工具容忍两者，好过让维护者在生成密钥时踩格式的坑。
//
// **不支持加密私钥**：Go 标准库没有安全的加密 PEM 解析（x509.DecryptPEMBlock
// 已废弃且算法过时），而私钥加密在"密钥只存于 GitHub Secrets"的模型下增益接近零，
// 见本文件头部说明。传入加密私钥会得到明确的报错而非静默失败。
func ParseECDSAPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "release sign: 私钥不是合法 PEM")
	}
	if _, encrypted := blk.Headers["DEK-Info"]; encrypted || blk.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, apperr.New(apperr.CodeInvalidInput,
			"release sign: 不支持加密私钥（Go 标准库无安全的加密 PEM 解析）。"+
				"请用 `openssl pkcs8 -topk8 -nocrypt` 生成未加密 PKCS#8 私钥并存入 GitHub Secrets——"+
				"Secrets 本身即静态加密，另加口令的防御增益接近零，详见 releasekeys/README.md")
	}

	if key, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("release sign: 需要 ECDSA 私钥，实际是 %T", key))
		}
		return ecKey, nil
	}
	// 回退 SEC1（openssl ecparam -genkey 的默认输出）
	ecKey, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput,
			"release sign: 私钥既非 PKCS#8 也非 SEC1 EC 格式", err)
	}
	return ecKey, nil
}

// PublicKeyPEMFromPrivate 从私钥导出 SPKI PEM 公钥，供生成密钥对时落盘到
// releasekeys/，以及流水线自验时与已提交公钥比对。
func PublicKeyPEMFromPrivate(priv *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "release sign: marshal public key", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
