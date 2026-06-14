package kernel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

type SessionPIIVault struct {
	db     *sql.DB
	encKey []byte
	mem    protocol.Memory
}

var _ PIIVaultRestorer = (*SessionPIIVault)(nil)

func NewSessionPIIVault(db *sql.DB, encKey []byte, mem protocol.Memory) *SessionPIIVault {
	return &SessionPIIVault{
		db:     db,
		encKey: encKey,
		mem:    mem,
	}
}

func (v *SessionPIIVault) Snapshot(ctx context.Context, taskID string, fields map[string]string) error {
	now := time.Now().UnixMilli()
	expiredAt := now + 3600000 // + 1 hour
	for key, val := range fields {
		dbKey := fmt.Sprintf("pii_vault:%s:%s", taskID, key)
		encVal, err := encryptFieldVault(v.encKey, val)
		if err != nil {
			return err
		}
		_, err = v.db.ExecContext(ctx, "INSERT OR REPLACE INTO preferences (key, value, expired_at) VALUES (?, ?, ?)", dbKey, encVal, expiredAt)
		if err != nil {
			return err
		}
	}
	return nil
}

func (v *SessionPIIVault) RestoreFromSnapshot(ctx context.Context, taskID string) error {
	now := time.Now().UnixMilli()
	rows, err := v.db.QueryContext(ctx, "SELECT key, value FROM preferences WHERE key LIKE ? AND (expired_at IS NULL OR expired_at > ?)", fmt.Sprintf("pii_vault:%s:%%", taskID), now)
	if err != nil {
		return err
	}
	defer rows.Close()

	if v.mem == nil || v.mem.Working() == nil {
		return errors.New("pii_vault: memory not available")
	}

	for rows.Next() {
		var k, val string
		if err := rows.Scan(&k, &val); err != nil {
			continue
		}
		decVal, err := decryptFieldVault(v.encKey, val)
		if err != nil {
			continue
		}
		field := strings.TrimPrefix(k, fmt.Sprintf("pii_vault:%s:", taskID))
		v.mem.Working().Scratch().Set(field, []byte(decVal))
	}
	return nil
}

func (v *SessionPIIVault) SecureZero(ctx context.Context, taskID string) error {
	_, err := v.db.ExecContext(ctx, "DELETE FROM preferences WHERE key LIKE ?", fmt.Sprintf("pii_vault:%s:%%", taskID))
	return err
}

func encryptFieldVault(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", errors.New("encryption key is missing")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func decryptFieldVault(key []byte, cryptoText string) (string, error) {
	if cryptoText == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", errors.New("encryption key is missing")
	}
	data, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := aesgcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
