package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// TestLoadFoundingAnchorSigningKey_Unset 2026-07-14 回归防护：VerifySignature
// 此前全仓零调用点的根因是 LoadOrCreate 恒传 nil privKey——未配置环境变量时
// 必须保持 (nil, nil)，对齐 founding_anchor.go 的"开发模式放行"语义。
func TestLoadFoundingAnchorSigningKey_Unset(t *testing.T) {
	t.Setenv("POLARIS_FOUNDING_ANCHOR_PRIVKEY", "")
	priv, pub := loadFoundingAnchorSigningKey()
	if priv != nil || pub != nil {
		t.Errorf("expected (nil, nil) when env unset, got priv=%v pub=%v", priv, pub)
	}
}

func TestLoadFoundingAnchorSigningKey_Invalid(t *testing.T) {
	t.Setenv("POLARIS_FOUNDING_ANCHOR_PRIVKEY", "not-valid-base64-!!!")
	priv, pub := loadFoundingAnchorSigningKey()
	if priv != nil || pub != nil {
		t.Errorf("expected (nil, nil) for invalid base64, got priv=%v pub=%v", priv, pub)
	}
}

func TestLoadFoundingAnchorSigningKey_WrongLength(t *testing.T) {
	// 合法 base64，但解码后长度不是 ed25519.PrivateKeySize（64 字节）。
	t.Setenv("POLARIS_FOUNDING_ANCHOR_PRIVKEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	priv, pub := loadFoundingAnchorSigningKey()
	if priv != nil || pub != nil {
		t.Errorf("expected (nil, nil) for wrong-length key, got priv=%v pub=%v", priv, pub)
	}
}

func TestLoadFoundingAnchorSigningKey_Valid(t *testing.T) {
	wantPub, wantPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	t.Setenv("POLARIS_FOUNDING_ANCHOR_PRIVKEY", base64.StdEncoding.EncodeToString(wantPriv))

	priv, pub := loadFoundingAnchorSigningKey()
	if priv == nil || pub == nil {
		t.Fatalf("expected non-nil (priv, pub) for valid key, got priv=%v pub=%v", priv, pub)
	}
	if !priv.Equal(wantPriv) {
		t.Error("returned private key does not match input")
	}
	if !pub.Equal(wantPub) {
		t.Error("returned public key does not match derived public key")
	}
}
