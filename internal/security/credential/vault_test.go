package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVault_EncryptDecrypt_RoundTrip 验证 AES-256-GCM 加解密往返一致性，
// 这是 vault 作为 Provider API Key 加密存储载体的核心正确性保证（P0-1）。
func TestVault_EncryptDecrypt_RoundTrip(t *testing.T) {
	v, err := NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey failed: %v", err)
	}

	cases := []string{
		"sk-abcdef0123456789",
		"",
		"包含中文的密钥",
		strings.Repeat("x", 4096), // 长文本
	}
	for _, plaintext := range cases {
		ciphertext, err := v.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q) failed: %v", plaintext, err)
		}
		if plaintext == "" {
			if ciphertext != "" {
				t.Errorf("Encrypt(\"\") expected empty ciphertext, got %q", ciphertext)
			}
			continue
		}
		if !strings.HasPrefix(ciphertext, "v1:") {
			t.Errorf("Encrypt(%q) ciphertext missing v1: prefix, got %q", plaintext, ciphertext)
		}
		got, err := v.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt(%q) failed: %v", ciphertext, err)
		}
		if got != plaintext {
			t.Errorf("round trip mismatch: want %q, got %q", plaintext, got)
		}
	}
}

// TestVault_Encrypt_NonDeterministic 验证每次 Encrypt 使用随机 nonce，
// 同一明文两次加密结果不同（否则相同 API Key 会产生可关联的固定密文，
// 存在侧信道风险）。
func TestVault_Encrypt_NonDeterministic(t *testing.T) {
	v, err := NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey failed: %v", err)
	}
	a, err := v.Encrypt("same-secret")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	b, err := v.Encrypt("same-secret")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if a == b {
		t.Errorf("expected distinct ciphertexts for repeated Encrypt calls, got identical: %q", a)
	}
}

// TestVault_Decrypt_PlaintextPassthrough 验证无 "v1:" 前缀的输入被当作历史明文
// 原样返回——这是从明文存储平滑迁移到加密存储的关键行为（老数据不会解密失败）。
func TestVault_Decrypt_PlaintextPassthrough(t *testing.T) {
	v, err := NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey failed: %v", err)
	}
	got, err := v.Decrypt("sk-plain-legacy-key")
	if err != nil {
		t.Fatalf("Decrypt of legacy plaintext failed: %v", err)
	}
	if got != "sk-plain-legacy-key" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

// TestVault_Decrypt_WrongKey 验证用错误的 master key 解密会失败而非返回垃圾数据
// （GCM 认证标签校验失败），防止静默数据损坏。
func TestVault_Decrypt_WrongKey(t *testing.T) {
	v1, _ := NewVaultWithKey(bytesFilled(1))
	v2, _ := NewVaultWithKey(bytesFilled(2))

	ciphertext, err := v1.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if _, err := v2.Decrypt(ciphertext); err == nil {
		t.Errorf("expected error decrypting with wrong key, got nil")
	}
}

// TestVault_Decrypt_TruncatedCiphertext 验证密文短于 nonce 长度时返回错误而非 panic。
func TestVault_Decrypt_TruncatedCiphertext(t *testing.T) {
	v, err := NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewVaultWithKey failed: %v", err)
	}
	if _, err := v.Decrypt("v1:AA=="); err == nil {
		t.Errorf("expected error for truncated ciphertext, got nil")
	}
}

// TestNewVaultWithKey_ShortKeyRejected 验证短于 32 字节的 key 被拒绝，
// 避免弱密钥静默生效。
func TestNewVaultWithKey_ShortKeyRejected(t *testing.T) {
	if _, err := NewVaultWithKey(make([]byte, 16)); err == nil {
		t.Errorf("expected error for short key, got nil")
	}
}

// TestNewVaultInDir_GeneratesAndPersistsKey 验证 NewVaultInDir 在 dataDir 下
// 首次调用自动生成 vault.key（而非依赖 home 目录），且二次调用复用同一份 key
// （否则每次重启都会生成新 key，导致已加密数据全部无法解密——这正是本次
// 修复的 P0-1 数据根目录一致性问题的回归测试）。
func TestNewVaultInDir_GeneratesAndPersistsKey(t *testing.T) {
	dataDir := t.TempDir()

	v1, err := NewVaultInDir(dataDir)
	if err != nil {
		t.Fatalf("NewVaultInDir (first call) failed: %v", err)
	}

	keyPath := filepath.Join(dataDir, "vault.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("expected vault.key to be created at %s: %v", keyPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected vault.key mode 0600, got %o", perm)
	}

	ciphertext, err := v1.Encrypt("provider-api-key")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// 二次调用必须读取已存在的 key，而不是重新生成。
	v2, err := NewVaultInDir(dataDir)
	if err != nil {
		t.Fatalf("NewVaultInDir (second call) failed: %v", err)
	}
	got, err := v2.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt with reloaded vault failed: %v", err)
	}
	if got != "provider-api-key" {
		t.Errorf("expected decrypted value to survive vault reload, got %q", got)
	}
}

// TestNewVaultInDir_PassphraseEnvOverride 验证 POLARIS_VAULT_PASSPHRASE 存在时
// 优先于 dataDir 下的 key 文件生效，且不会在磁盘上创建 vault.key。
func TestNewVaultInDir_PassphraseEnvOverride(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("POLARIS_VAULT_PASSPHRASE", "test-passphrase")

	v, err := NewVaultInDir(dataDir)
	if err != nil {
		t.Fatalf("NewVaultInDir failed: %v", err)
	}
	ciphertext, err := v.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	got, err := v.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if got != "secret" {
		t.Errorf("round trip mismatch: got %q", got)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "vault.key")); !os.IsNotExist(err) {
		t.Errorf("expected no vault.key file to be written when passphrase env is set")
	}
}

// TestNewVault_DefaultsUnderHomeDir 验证不带参数的 NewVault() 仍然可用
// （向后兼容路径），key 落在 ~/.polarisagi/polaris/vault.key。
func TestNewVault_DefaultsUnderHomeDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("POLARIS_VAULT_PASSPHRASE", "") // 确保走文件 key 路径

	v, err := NewVault()
	if err != nil {
		t.Fatalf("NewVault failed: %v", err)
	}
	expected := filepath.Join(tmpHome, ".polarisagi", "polaris", "vault.key")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected vault.key at %s: %v", expected, err)
	}

	ciphertext, err := v.Encrypt("x")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if _, err := v.Decrypt(ciphertext); err != nil {
		t.Errorf("Decrypt failed: %v", err)
	}
}

// TestGenerateNewKey 验证 GenerateNewKey 用于密钥轮换场景：生成新 32 字节随机
// key 并落盘，返回值与文件内容一致。
func TestGenerateNewKey(t *testing.T) {
	dataDir := t.TempDir()
	keyPath := filepath.Join(dataDir, "vault.key.new")

	key, err := GenerateNewKey(keyPath)
	if err != nil {
		t.Fatalf("GenerateNewKey failed: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte key, got %d bytes", len(key))
	}
	onDisk, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("failed to read generated key file: %v", err)
	}
	if string(onDisk) != string(key) {
		t.Errorf("returned key does not match on-disk key")
	}
}

// bytesFilled 返回长度 32、所有字节等于 b 的切片，便于构造两把互不相同的测试 key。
func bytesFilled(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}
