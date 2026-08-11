package updater

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// TestSignVerifyRoundTrip 锁死签名侧与验签侧的格式对齐。
//
// 这是本特性最关键的一条：签名侧（SignChecksumFile）与验签侧（verifyWithKeys）
// 分别由流水线和客户端执行，隔着一次发布才会碰面。格式对不上的后果是"发出去的
// 包所有客户端都装不上"，而且要等到真发版才暴露。故在此把两侧的 round-trip
// 钉死在单测里。
func TestSignVerifyRoundTrip(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	payload := []byte("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  polaris-linux-amd64.tar.gz\n")

	for _, tc := range []struct {
		name    string
		keyPEM  []byte
		wantErr bool
	}{
		{name: "PKCS#8 私钥", keyPEM: pkcs8PEM(t, priv)},
		{name: "SEC1 私钥", keyPEM: sec1PEM(t, priv)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig, err := SignChecksumFile(tc.keyPEM, payload)
			if err != nil {
				t.Fatalf("签名失败: %v", err)
			}
			keys := []releaseKey{{name: "release-test.pem", pub: &priv.PublicKey}}
			if err := verifyWithKeys(keys, payload, sig); err != nil {
				t.Fatalf("签名侧产出无法被验签侧验证——两侧格式已漂移: %v", err)
			}
			// 篡改后必须验不过，确认不是"恒真"的假通过。
			if err := verifyWithKeys(keys, append(payload, 'x'), sig); err == nil {
				t.Fatal("篡改 payload 后仍验签通过，验签逻辑失效")
			}
		})
	}
}

// TestParseECDSAPrivateKeyPEM_Rejects 守护私钥解析的三条拒绝路径。
func TestParseECDSAPrivateKeyPEM_Rejects(t *testing.T) {
	t.Run("非 PEM", func(t *testing.T) {
		if _, err := ParseECDSAPrivateKeyPEM([]byte("not pem")); err == nil {
			t.Fatal("非 PEM 应被拒绝")
		}
	})

	t.Run("加密私钥给出可操作的报错", func(t *testing.T) {
		// Go 标准库没有安全的加密 PEM 解析，必须明确报错而非静默失败。
		// 报错要指出正确的生成命令，否则维护者只会看到"解析失败"四个字。
		enc := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})
		_, err := ParseECDSAPrivateKeyPEM(enc)
		if err == nil {
			t.Fatal("加密私钥应被拒绝")
		}
		for _, want := range []string{"nocrypt", "Secrets"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("报错缺少处置指引 %q，实际: %v", want, err)
			}
		}
	})

	t.Run("非 ECDSA 私钥", func(t *testing.T) {
		// RSA 私钥是合法 PKCS#8，但本实现只接受 ECDSA——收窄可避免
		// "换了算法而验签代码没跟上"这类静默弱化。
		if _, err := ParseECDSAPrivateKeyPEM(rsaPKCS8PEM(t)); err == nil {
			t.Fatal("非 ECDSA 私钥应被拒绝")
		}
	})
}

// TestPublicKeyPEMFromPrivate 导出的公钥必须能被验签侧的解析器读回。
func TestPublicKeyPEMFromPrivate(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	pubPEM, err := PublicKeyPEMFromPrivate(priv)
	if err != nil {
		t.Fatalf("导出公钥失败: %v", err)
	}
	got, err := parseECDSAPublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("导出的公钥无法被 releasekeys 解析器读回: %v", err)
	}
	if !got.Equal(&priv.PublicKey) {
		t.Fatal("导出的公钥与私钥不配对")
	}
}

func pkcs8PEM(t *testing.T, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func sec1PEM(t *testing.T, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal sec1: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func rsaPKCS8PEM(t *testing.T) []byte {
	t.Helper()
	// 本用例只验"非 ECDSA 会被拒"，与签名强度无关；用 2048 位是因为 Go 1.24+
	// 的 crypto/rsa 拒绝生成小于 1024 位的密钥，2048 是最省事的合规取值。
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal rsa pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
