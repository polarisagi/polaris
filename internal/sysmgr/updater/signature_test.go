package updater

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

// 本文件守护 ADR-0095 决策二的客户端侧：验签逻辑必须与 cosign sign-blob 的产物
// 格式严格对齐，且信任根为空/被篡改时的行为必须是确定的。
//
// 用 stdlib 现场生成密钥对来构造"cosign 格式"的签名：cosign sign-blob 对
// ECDSA 密钥做的正是 ecdsa.SignASN1(sha256(payload)) 再 base64，二者字节级等价。
// 这样测试不依赖 CI 安装 cosign，也不需要往仓库里塞任何私钥。

// signLikeCosign 模拟 `cosign sign-blob --key <ecdsa> --output-signature`。
func signLikeCosign(t *testing.T, priv *ecdsa.PrivateKey, payload []byte) string {
	t.Helper()
	digest := sha256.Sum256(payload)
	der, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// pubKeyPEM 生成 cosign.pub 同格式的 SPKI PEM。
func pubKeyPEM(t *testing.T, pub *ecdsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("序列化公钥失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// ed25519PubPEM 生成一个合法但**非 ECDSA** 的 SPKI PEM，用于验证类型收窄生效。
func ed25519PubPEM(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 Ed25519 密钥失败: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("序列化 Ed25519 公钥失败: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestParseECDSAPublicKeyPEM(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}

	t.Run("合法 cosign.pub 可解析", func(t *testing.T) {
		got, err := parseECDSAPublicKeyPEM(pubKeyPEM(t, &priv.PublicKey))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !got.Equal(&priv.PublicKey) {
			t.Fatal("解析出的公钥与原公钥不一致")
		}
	})

	t.Run("非 PEM 内容被拒绝", func(t *testing.T) {
		if _, err := parseECDSAPublicKeyPEM([]byte("not a pem")); err == nil {
			t.Fatal("非 PEM 内容应被拒绝")
		}
	})

	t.Run("非 ECDSA 公钥被拒绝", func(t *testing.T) {
		// 只接受 ECDSA 是刻意收窄：换算法而验签代码没跟上属于静默弱化。
		// 用 Ed25519 公钥（同样是合法 SPKI）验证这条收窄真的生效。
		edPEM := ed25519PubPEM(t)
		if _, err := parseECDSAPublicKeyPEM(edPEM); err == nil {
			t.Fatal("Ed25519 公钥应被拒绝（本实现只接受 ECDSA）")
		}
	})
}

func TestVerifyReleaseSignature(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	payload := []byte("2f0e...  polaris-linux-amd64.tar.gz\n")

	// 直接复用生产验签核心，只替换信任根——不改全局 trustStore，
	// 避免测试之间互相污染（trustStore 是 sync.OnceValue，改不回去）。
	verifyWith := verifyWithKeys

	trusted := []releaseKey{{name: "release-test.pem", pub: &priv.PublicKey}}

	t.Run("正确签名通过", func(t *testing.T) {
		if err := verifyWith(trusted, payload, signLikeCosign(t, priv, payload)); err != nil {
			t.Fatalf("合法签名应通过: %v", err)
		}
	})

	t.Run("篡改 payload 被拒绝", func(t *testing.T) {
		sig := signLikeCosign(t, priv, payload)
		tampered := []byte("dead...  polaris-linux-amd64.tar.gz\n")
		if err := verifyWith(trusted, tampered, sig); err == nil {
			t.Fatal("payload 被篡改后必须验签失败——这正是签名要拦的攻击")
		}
	})

	t.Run("他人密钥签名被拒绝", func(t *testing.T) {
		if err := verifyWith(trusted, payload, signLikeCosign(t, other, payload)); err == nil {
			t.Fatal("非信任根签发的签名必须被拒绝")
		}
	})

	t.Run("轮换期新旧公钥并存_两者均通过", func(t *testing.T) {
		// 轮换必需的性质：信任根同时含新旧公钥时，任一把签的都认。
		rotating := []releaseKey{
			{name: "release-2026.pem", pub: &priv.PublicKey},
			{name: "release-2027.pem", pub: &other.PublicKey},
		}
		if err := verifyWith(rotating, payload, signLikeCosign(t, priv, payload)); err != nil {
			t.Fatalf("旧公钥签名在轮换期应仍被接受: %v", err)
		}
		if err := verifyWith(rotating, payload, signLikeCosign(t, other, payload)); err != nil {
			t.Fatalf("新公钥签名应被接受: %v", err)
		}
	})

	t.Run("非 base64 签名被拒绝", func(t *testing.T) {
		if err := verifyWith(trusted, payload, "!!!not-base64!!!"); err == nil {
			t.Fatal("非 base64 签名应被拒绝")
		}
	})

	t.Run("信任根为空时拒绝而非放行", func(t *testing.T) {
		// fail-closed：空信任根下"验签"不能返回 nil，否则调用方会误以为验过了。
		// 「未开通签名」这一降级判断由 anchorChecksumTrust 显式做，不能藏在这里。
		err := verifyWith(nil, payload, signLikeCosign(t, priv, payload))
		if err == nil {
			t.Fatal("信任根为空时必须返回错误")
		}
		if !strings.Contains(err.Error(), "trust store is empty") {
			t.Errorf("错误信息应指明信任根为空，实际: %v", err)
		}
	})
}

// TestEmbeddedTrustStoreParses 守护 releasekeys/ 目录：放进去的 .pem 必须全部
// 解析成功。loadTrustStore 对坏文件是静默跳过（一个坏文件不该让整个信任根失效），
// 因此需要本测试在 CI 阶段把"提交了一个格式不对的公钥"暴露出来——否则会表现为
// 线上悄悄少了一把可信公钥，直到某次轮换后所有客户端集体验签失败才被发现。
func TestEmbeddedTrustStoreParses(t *testing.T) {
	entries, err := releaseKeysFS.ReadDir("releasekeys")
	if err != nil {
		t.Fatalf("读取内嵌 releasekeys 目录失败: %v", err)
	}
	pemCount := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		pemCount++
		raw, readErr := releaseKeysFS.ReadFile("releasekeys/" + e.Name())
		if readErr != nil {
			t.Errorf("%s 读取失败: %v", e.Name(), readErr)
			continue
		}
		if _, parseErr := parseECDSAPublicKeyPEM(raw); parseErr != nil {
			t.Errorf("%s 不是合法的 ECDSA SPKI PEM 公钥（cosign generate-key-pair 产出的 cosign.pub 格式）: %v",
				e.Name(), parseErr)
		}
	}
	if got := len(trustStore()); got != pemCount {
		t.Errorf("信任根装载数 %d ≠ 目录内 .pem 数 %d —— 有公钥被静默跳过", got, pemCount)
	}
	if pemCount == 0 {
		t.Log("releasekeys/ 内暂无公钥：发布签名尚未开通，updater 将退回纯 checksum 校验并告警。" +
			"开通流程见 internal/sysmgr/updater/releasekeys/README.md")
	}
}
