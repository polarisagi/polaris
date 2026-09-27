package server

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// newVaultMasterKeyRotator 构造一个 vault 主密钥轮换闭包，供
// sysadmin.SysAdminHandler.RotateVaultMasterKey 使用（ADR-0096 决策一修复：
// rotate-master-key 此前由 cmd/polaris/cli_vault.go 直连 SQLite 完成，绕过
// 守护进程，与 HTTP-only CLI 纪律冲突）。
//
// rwDB/oldVault/dataDir 通过闭包捕获而非存成 sysadmin 包的结构体字段——
// 轮换是一次性管理动作，不值得让 sysadmin 持有写库句柄与密钥。
//
// 轮换流程：生成新 key → 单事务内用旧 vault 解密、新 vault 重新加密全部密文列
// （repo.RekeyVaultCiphertexts）→ 提交 → 原子替换 vault.key。调用方（HandleVaultRotateMasterKey）负责
// 轮换成功后重启进程：进程内其余持有旧 masterKey 的
// credential.Vault 实例（Notion token、ReloadProviders、MCP OAuth 等）
// vault.key 重新加载是唯一能保证不留不一致状态的做法。
//
// 为什么必须单事务：此前逐条 UPDATE providers，中途失败时已改写的行用的是随后被丢弃的新 key，
// 永久无法解密；且只覆盖 providers，插件敏感配置与 MCP OAuth 令牌在轮换后全部失效。
// 返回值为轮换的 provider 数（HTTP 响应字段 providers_rotated 的语义保持不变）。
func newVaultMasterKeyRotator(rwDB *sql.DB, oldVault *credential.Vault, dataDir string) func(ctx context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		keyPath := filepath.Join(dataDir, "vault.key")
		newKeyPath := keyPath + ".new"

		newKey, err := credential.GenerateNewKey(newKeyPath)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rotate: generate new key failed", err)
		}
		newVault, err := credential.NewVaultWithKey(newKey)
		if err != nil {
			os.Remove(newKeyPath) //nolint:errcheck // 生成新 vault 失败，清理半成品密钥文件
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rotate: load new vault failed", err)
		}

		counts, err := repo.RekeyVaultCiphertexts(ctx, rwDB, func(ct string) (string, error) {
			plain, err := oldVault.Decrypt(ct)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeInternal, "vault rotate: decrypt", err)
			}
			out, err := newVault.Encrypt(plain)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeInternal, "vault rotate: encrypt", err)
			}
			return out, nil
		})
		if err != nil {
			// 事务已回滚，旧 vault.key 仍是全部密文的真实密钥，不落地半成品新 key。
			os.Remove(newKeyPath) //nolint:errcheck // 轮换失败回滚，清理半成品密钥文件
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rotate: re-encrypt failed", err)
		}
		if err := os.Rename(newKeyPath, keyPath); err != nil {
			return counts.Providers, apperr.Wrap(apperr.CodeInternal, "vault rotate: swap key file failed", err)
		}
		slog.Info("vault rotate: ciphertexts re-encrypted", "providers", counts.Providers, "other_secrets", counts.Others)
		return counts.Providers, nil
	}
}
