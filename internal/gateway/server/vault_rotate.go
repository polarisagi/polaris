package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// newVaultMasterKeyRotator 构造一个 vault 主密钥轮换闭包，供
// sysadmin.SysAdminHandler.RotateVaultMasterKey 使用（ADR-0096 决策一修复：
// rotate-master-key 此前由 cmd/polaris/cli_vault.go 直连 SQLite 完成，绕过
// "客户端与内核之间只有 HTTP 一条路"这条唯一业务通道）。
//
// rwDB/oldVault/dataDir 通过闭包捕获而非存成 sysadmin 包的结构体字段——
// inv_NoRawSQLDBField 禁止 storage 层外的包声明 *sql.DB 字段，本函数所在的
// internal/gateway/server 顶层包持有它们的方式与 NewServer 构造其余 repo
// 时完全一致（函数参数/局部变量，不是字段）。
//
// 轮换流程：生成新 key → 用旧 vault 解密全部 provider API Key → 用新 vault
// 重新加密写回 → 原子替换 vault.key。调用方（HandleVaultRotateMasterKey）负责
// 在轮换成功后触发进程重启——本进程内除这条路径外还有其他独立构造的
// credential.Vault 实例（Notion token、ReloadProviders 等）持有旧 masterKey
// 的内存副本，轮换完成到进程重启之间它们仍会用旧 key 读写，重启后统一从新
// vault.key 重新加载是唯一能保证不留不一致状态的做法。
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

		oldRepo := repo.NewSQLiteProviderRepository(rwDB).WithVault(oldVault)
		newRepo := repo.NewSQLiteProviderRepository(rwDB).WithVault(newVault)

		providers, err := oldRepo.ListProviders(ctx)
		if err != nil {
			os.Remove(newKeyPath) //nolint:errcheck // 轮换失败回滚，清理半成品密钥文件
			return 0, apperr.Wrap(apperr.CodeInternal, "vault rotate: list providers failed", err)
		}

		rotated := 0
		for _, p := range providers {
			if p.APIKey == "" {
				continue
			}
			if err := newRepo.UpdateProviderAPIKey(ctx, p.ID, p.APIKey, p.UpdatedAt); err != nil {
				// 轮换中途失败，旧 vault.key 仍是当前密文的真实密钥，不落地半成品新 key。
				os.Remove(newKeyPath) //nolint:errcheck
				return rotated, apperr.Wrap(apperr.CodeInternal, "vault rotate: update provider "+p.ID+" failed", err)
			}
			rotated++
		}

		if err := os.Rename(newKeyPath, keyPath); err != nil {
			return rotated, apperr.Wrap(apperr.CodeInternal, "vault rotate: swap key file failed", err)
		}
		return rotated, nil
	}
}
