package config

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

func TestVerifyBinarySeal_NoSidecar(t *testing.T) {
	// 无 sidecar 时应放行（开发模式）
	if err := verifyBinarySeal(); err != nil {
		t.Fatalf("无 sidecar 时不应返回 error，got: %v", err)
	}
}

func TestVerifyBinarySeal_Mismatch(t *testing.T) {
	exe, _ := os.Executable()
	sidecar := exe + ".sha256"

	// 写入错误哈希
	_ = os.WriteFile(sidecar, []byte("0000000000000000000000000000000000000000000000000000000000000000"), 0o600)
	defer os.Remove(sidecar)

	err := verifyBinarySeal()
	if err == nil {
		t.Fatal("哈希不匹配时应返回 error")
	}
}

func TestVerifyBinarySeal_Correct(t *testing.T) {
	exe, _ := os.Executable()
	f, _ := os.Open(exe)
	defer f.Close()
	h := sha256.New()
	_, _ = io.Copy(h, f)
	correctHash := hex.EncodeToString(h.Sum(nil))

	sidecar := exe + ".sha256"
	_ = os.WriteFile(sidecar, []byte(correctHash), 0o600)
	defer os.Remove(sidecar)

	if err := verifyBinarySeal(); err != nil {
		t.Fatalf("正确哈希时不应 error，got: %v", err)
	}
}
