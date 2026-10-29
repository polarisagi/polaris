package agentctx

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// piiVaultTTL 快照存活期：仅覆盖 provider 熔断恢复窗口，过期行由 Load/Restore 忽略。
const piiVaultTTL = time.Hour

// SessionPIIVault 崩溃/挂起恢复用的 PII 原文快照，落独立表 task_pii_vault（046），
// 不得再挪用 preferences——后者经 /preferences 系 API 外露密文与 task_id（ADR-0104 决策八）。
type SessionPIIVault struct {
	db     protocol.SQLQuerier
	encKey []byte
}

func NewSessionPIIVault(db protocol.SQLQuerier, encKey []byte) *SessionPIIVault {
	return &SessionPIIVault{db: db, encKey: encKey}
}

func (v *SessionPIIVault) Snapshot(ctx context.Context, taskID string, fields map[string]string) error {
	now := time.Now().UnixMilli()
	expiredAt := now + piiVaultTTL.Milliseconds()
	for key, val := range fields {
		encVal, err := encryptFieldVault(v.encKey, val)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.Snapshot", err)
		}
		_, err = v.db.ExecContext(ctx,
			"INSERT OR REPLACE INTO task_pii_vault (task_id, field, enc_value, expired_at, created_at) VALUES (?, ?, ?, ?, ?)",
			taskID, key, encVal, expiredAt, now)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.Snapshot", err)
		}
	}
	return nil
}

// Load 解密并返回任务的未过期快照字段；无快照返回空 map。
// 单行解密失败跳过并告警（密钥轮换后旧密文不可读不应阻断其余字段）。
func (v *SessionPIIVault) Load(ctx context.Context, taskID string) (map[string]string, error) {
	rows, err := v.db.QueryContext(ctx,
		"SELECT field, enc_value FROM task_pii_vault WHERE task_id = ? AND (expired_at IS NULL OR expired_at > ?)",
		taskID, time.Now().UnixMilli())
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.Load", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var field, enc string
		if err := rows.Scan(&field, &enc); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.Load", err)
		}
		dec, err := decryptFieldVault(v.encKey, enc)
		if err != nil {
			slog.Warn("pii_vault: decrypt failed, skip field", "task_id", taskID, "field", field, "err", err)
			continue
		}
		out[field] = dec
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.Load", err)
	}
	return out, nil
}

// RestoreFromSnapshot 仅校验快照仍在。恢复后的任务经 Blackboard 重新领取，意图取自
// tasks 表而非本快照，全仓无消费方需要 raw_intent/session_id 明文；此前写入进程级
// 共享 Scratch 的路径无读取者且会跨任务串扰，已删除。快照缺失只告警：
// 不阻断唤醒，过期清理由 SecureZero 在终态负责。
func (v *SessionPIIVault) RestoreFromSnapshot(ctx context.Context, taskID string) error {
	var n int
	err := v.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_pii_vault WHERE task_id = ? AND (expired_at IS NULL OR expired_at > ?)",
		taskID, time.Now().UnixMilli()).Scan(&n)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.RestoreFromSnapshot", err)
	}
	if n == 0 {
		slog.Warn("pii_vault: no live snapshot for recovered task", "task_id", taskID)
	}
	return nil
}

func (v *SessionPIIVault) SecureZero(ctx context.Context, taskID string) error {
	_, err := v.db.ExecContext(ctx, "DELETE FROM task_pii_vault WHERE task_id = ?", taskID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SessionPIIVault.SecureZero", err)
	}
	return nil
}

func encryptFieldVault(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "encryption key is missing")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "encryptFieldVault", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "encryptFieldVault", err)
	}
	nonce := make([]byte, aesgcm.NonceSize()) //nolint:prealloc
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "encryptFieldVault", err)
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func decryptFieldVault(key []byte, cryptoText string) (string, error) {
	if cryptoText == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "encryption key is missing")
	}
	data, err := base64.StdEncoding.DecodeString(cryptoText)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "decryptFieldVault", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "decryptFieldVault", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "decryptFieldVault", err)
	}
	nonceSize := aesgcm.NonceSize()
	if len(data) < nonceSize {
		return "", apperr.New(apperr.CodeInvalidInput, "ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "decryptFieldVault", err)
	}
	return string(plaintext), nil
}
