package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Vault handles persistent encryption and decryption of credentials like API keys.
type Vault struct {
	masterKey []byte
}

// NewVault initializes a new Vault.
// Master key derivation strategy:
// 1. Reads from POLARIS_VAULT_PASSPHRASE env var (sha256 to ensure 32 bytes).
// 2. Otherwise reads from ~/.polarisagi/polaris/vault.key.
// 3. If file doesn't exist, it creates a new random key and saves it.
func NewVault() (*Vault, error) {
	if envKey := os.Getenv("POLARIS_VAULT_PASSPHRASE"); envKey != "" {
		hash := sha256.Sum256([]byte(envKey))
		return &Vault{masterKey: hash[:]}, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "NewVault: get home dir", err)
	}
	keyPath := filepath.Join(homeDir, ".polarisagi", "polaris", "vault.key")

	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		// Auto-generate if not exists
		if err := generateAndSaveKey(keyPath); err != nil {
			return nil, err
		}
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "NewVault: read key file", err)
	}
	if len(keyData) < 32 {
		return nil, apperr.New(apperr.CodeInternal, "NewVault: key file too short, must be at least 32 bytes")
	}

	return &Vault{masterKey: keyData[:32]}, nil
}

func generateAndSaveKey(keyPath string) error {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "NewVault: mkdir", err)
	}
	newKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, newKey); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "NewVault: generate key", err)
	}
	if err := os.WriteFile(keyPath, newKey, 0600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "NewVault: write key file", err)
	}
	return nil
}

// Encrypt encrypts the plaintext using AES-256-GCM.
func (v *Vault) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Encrypt", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Encrypt", err)
	}
	nonceSize := aesgcm.NonceSize()
	plaintextBytes := []byte(plaintext)
	out := make([]byte, nonceSize, nonceSize+len(plaintextBytes)+aesgcm.Overhead())
	if _, err := io.ReadFull(rand.Reader, out[:nonceSize]); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Encrypt", err)
	}
	result := aesgcm.Seal(out[:nonceSize], out[:nonceSize], plaintextBytes, nil)
	return "v1:" + base64.StdEncoding.EncodeToString(result), nil
}

// Decrypt decrypts the cryptoText using AES-256-GCM.
func (v *Vault) Decrypt(cryptoText string) (string, error) {
	if cryptoText == "" {
		return "", nil
	}
	if !strings.HasPrefix(cryptoText, "v1:") {
		// Return as-is if not encrypted (seamless migration)
		return cryptoText, nil
	}
	cryptoText = strings.TrimPrefix(cryptoText, "v1:")
	data, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Decrypt", err)
	}
	block, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Decrypt", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Decrypt", err)
	}
	nonceSize := aesgcm.NonceSize()
	if len(data) < nonceSize {
		return "", apperr.New(apperr.CodeInvalidInput, "ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "Vault.Decrypt", err)
	}
	return string(plaintext), nil
}

// GenerateNewKey writes a new random key to the specified path and returns it.
func GenerateNewKey(keyPath string) ([]byte, error) {
	if err := generateAndSaveKey(keyPath); err != nil {
		return nil, err
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	if len(keyData) < 32 {
		return nil, apperr.New(apperr.CodeInternal, "generated key too short")
	}
	return keyData[:32], nil
}

// NewVaultWithKey initializes a Vault with an explicit key (useful for rotation).
func NewVaultWithKey(key []byte) (*Vault, error) {
	if len(key) < 32 {
		return nil, apperr.New(apperr.CodeInternal, "key too short")
	}
	k := make([]byte, 32)
	copy(k, key[:32])
	return &Vault{masterKey: k}, nil
}
