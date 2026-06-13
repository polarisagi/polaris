package eval

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"testing"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

func TestVerifyEvalSignature(t *testing.T) {
	agentRole := "M9_OPTIMIZER"
	envKey := "POLARIS_EVAL_PUBKEY_M9_OPTIMIZER"

	// 1. 未配置公钥，放行
	os.Unsetenv(envKey)
	err := verifyEvalSignature(agentRole, "training", nil)
	if err != nil {
		t.Fatalf("expected nil when no pubkey, got %v", err)
	}

	// 2. 配置公钥
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Setenv(envKey, base64.StdEncoding.EncodeToString(pub))
	defer os.Unsetenv(envKey)

	// nil 签名
	err = verifyEvalSignature(agentRole, "training", nil)
	if err == nil {
		t.Fatal("expected error with nil signature when pubkey configured")
	}
	if err.(*perrors.Error).Code != perrors.CodeForbidden {
		t.Fatalf("expected CodeForbidden, got %v", err)
	}

	// 错误签名
	badSig := make([]byte, ed25519.SignatureSize)
	err = verifyEvalSignature(agentRole, "training", badSig)
	if err == nil {
		t.Fatal("expected error with bad signature")
	}

	// 正确签名
	now := time.Now().UTC()
	payload := []byte(agentRole + ":training:" + now.Format("200601021504"))
	sig := ed25519.Sign(priv, payload)

	err = verifyEvalSignature(agentRole, "training", sig)
	if err != nil {
		t.Fatalf("expected nil with valid signature, got %v", err)
	}
}
