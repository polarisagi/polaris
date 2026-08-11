// release 产物签名验证（ADR-0095 决策二）。
//
// 信任模型：ECDSA P-256 固定密钥对。私钥只存在于 GitHub Secrets，签名在 release
// 流水线由 `cosign sign-blob` 完成；公钥集内嵌在二进制里（releasekeys/*.pem），
// 验证走 Go 标准库，**不引入任何新依赖**。
//
// 为什么不用 cosign keyless（Fulcio/Rekor）——这是本决策唯一真正的取舍点：
// keyless 免去长期私钥管理，但客户端离线验证需要 `sigstore-go`。2026-08-10 实测
// （见 ADR-0095 决策二）：sigstore-go v1.3.0 引入 **368 个传递模块**，最小验签
// 程序 **16.6 MB**（同等 stdlib 实现 2.6 MB，即 Go 运行时基线）。当前 polaris
// 是 111 个模块 / 31.4 MB，接入后变成约 479 个模块 / 45 MB。
// 用"为修供应链问题而新增 368 个未审计的传递依赖"来换"免去一把私钥的管理"，
// 在本项目（[Tier-0-Limit] 2GB VPS 可运行、依赖表刻意精简）上是净负收益。
//
// 签名格式与 cosign 完全兼容，用户可独立自验，不被本实现绑架：
//
//	cosign verify-blob --key cosign.pub --signature polaris-linux-amd64.tar.gz.sha256.sig \
//	    polaris-linux-amd64.tar.gz.sha256
package updater

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/fs"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// releaseKeysFS 内嵌发布签名公钥集。
//
// 目录内除 .pem 外还有 README.md（开通与轮换流程），故 embed 整个目录而非
// `releasekeys/*.pem`——后者在目录里一个 .pem 都没有时会**编译失败**，而
// "尚未开通签名"恰恰是本特性落地初期的正常状态（见 loadTrustStore 注释）。
//
//go:embed releasekeys
var releaseKeysFS embed.FS

// trustStore 解析并返回内嵌可信公钥集。
//
// 每次调用重新解析而非缓存进包级变量：调用点只有 Manager 构造与 CLI 自检
// （每进程个位数次），解析几个几百字节的 PEM 是微秒级开销；而包级缓存会引入
// 一个 `internal/` 明令禁止的全局可变变量（CLAUDE.md R1.3），为省这点开销
// 去申请 lint 豁免不划算。Manager 在 New 时把结果快照进 releaseKeys 字段，
// 更新热路径上不会重复解析。
func trustStore() []releaseKey { return loadTrustStore() }

// releaseKey 一条可信公钥及其来源文件名（文件名仅用于日志辨识轮换代次）。
type releaseKey struct {
	name string
	pub  *ecdsa.PublicKey
}

// loadTrustStore 解析 releasekeys/*.pem。
//
// 返回空切片是**合法状态**，语义是"发布签名尚未开通"：仓库刚落地本特性时还没有
// 密钥对（私钥必须由仓库管理员生成并存入 GitHub Secrets，代码无法代劳）。
// 此时 verifyReleaseSignature 的调用方退回纯 checksum 校验并告警，
// 与本特性落地前的行为一致；一旦有人把 .pem 提交进来，客户端自动转 fail-closed。
//
// 解析失败的文件被跳过而非 panic：一个坏文件不该让整个信任根失效，
// 但会计入 skipped 供 TestEmbeddedTrustStoreParses 与启动日志暴露。
func loadTrustStore() []releaseKey {
	entries, err := fs.ReadDir(releaseKeysFS, "releasekeys")
	if err != nil {
		return nil
	}
	var keys []releaseKey
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		raw, readErr := releaseKeysFS.ReadFile("releasekeys/" + e.Name())
		if readErr != nil {
			continue
		}
		pub, parseErr := parseECDSAPublicKeyPEM(raw)
		if parseErr != nil {
			continue
		}
		keys = append(keys, releaseKey{name: e.Name(), pub: pub})
	}
	return keys
}

// parseECDSAPublicKeyPEM 解析 SPKI 格式的 PEM 公钥（cosign.pub 即为此格式）。
func parseECDSAPublicKeyPEM(raw []byte) (*ecdsa.PublicKey, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "release key: not a PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "release key: parse PKIX public key", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		// 只接受 ECDSA：cosign 默认生成 ECDSA P-256，收窄类型可避免
		// "换了个算法但验签代码没跟上"这类静默弱化。
		return nil, apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("release key: expected ECDSA public key, got %T", parsed))
	}
	return pub, nil
}

// SigningProvisioned 报告发布签名是否已开通（内嵌信任根非空）。
// 导出供 `polaris release-key` CLI 与运维自检使用。
func SigningProvisioned() bool { return len(trustStore()) > 0 }

// VerifyReleaseSignature 是 verifyReleaseSignature 的导出别名，供 CLI 离线自验
// 手工下载的 release 产物（无需安装 cosign）。
func VerifyReleaseSignature(payload []byte, sigB64 string) error {
	return verifyReleaseSignature(payload, sigB64)
}

// TrustStoreFingerprints 返回内嵌公钥的 "文件名 指纹" 列表，供 CLI 展示。
func TrustStoreFingerprints() []string { return trustStoreFingerprints() }

// verifyReleaseSignature 用内嵌公钥集验证 payload 的 cosign 签名。
//
// sigB64 是 `cosign sign-blob --output-signature` 的产物：DER 编码的 ECDSA 签名
// 再做 base64。cosign 签的是 payload 的 SHA-256 摘要，故此处同样先取摘要。
//
// 遍历全部公钥逐个尝试，任一通过即成立——这是密钥轮换期新旧公钥并存所必需的。
func verifyReleaseSignature(payload []byte, sigB64 string) error {
	return verifyWithKeys(trustStore(), payload, sigB64)
}

// verifyWithKeys 是验签核心，信任根由参数传入。
//
// 与 verifyReleaseSignature 分开是为了让单测能注入临时密钥对：trustStore 是
// sync.OnceValue，一旦求值就改不回去，测试直接改全局会互相污染。
func verifyWithKeys(keys []releaseKey, payload []byte, sigB64 string) error {
	// 信任根为空时返回错误而非放行：fail-closed。
	// "签名未开通所以跳过验签"是一个业务降级判断，必须由调用方
	// （anchorChecksumTrust）显式做出并留痕，不能藏在验签函数里变成静默通过。
	if len(keys) == 0 {
		return apperr.New(apperr.CodeInternal, "release signature: trust store is empty (signing not provisioned)")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "release signature: not valid base64", err)
	}
	digest := sha256.Sum256(payload)
	for _, k := range keys {
		if ecdsa.VerifyASN1(k.pub, digest[:], sig) {
			return nil
		}
	}
	return apperr.New(apperr.CodeInternal, fmt.Sprintf(
		"release signature: verification FAILED against all %d trusted key(s) — "+
			"产物可能被篡改，或签名密钥已轮换而本二进制过旧（升级后重试）", len(keys)))
}

// trustStoreFingerprints 返回各公钥的 SHA-256 指纹（前 16 hex），供 CLI 展示与
// 日志辨识。指纹取自 SPKI DER 字节，与 `openssl pkey -pubin -outform DER | sha256sum`
// 一致，便于运维用系统工具交叉核对。
func trustStoreFingerprints() []string {
	keys := trustStore()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		der, err := x509.MarshalPKIXPublicKey(k.pub)
		if err != nil {
			out = append(out, k.name+" <marshal failed>")
			continue
		}
		sum := sha256.Sum256(der)
		out = append(out, fmt.Sprintf("%s %x", k.name, sum[:8]))
	}
	return out
}
