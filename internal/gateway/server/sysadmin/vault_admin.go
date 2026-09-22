package sysadmin

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/security/credential"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HandleVaultRotateMasterKey 轮换 credential.Vault 主密钥：生成新密钥 → 用旧密钥解密、
// 新密钥重新加密全部 provider API Key → 原子替换 vault.key → 重启守护进程（从新 key
// 重新加载 Vault）。
//
// 迁移背景（ADR-0096 决策一）：此前 `polaris vault rotate-master-key` 由
// cmd/polaris/cli_vault.go 直连 SQLite 完成，绕过"客户端与内核之间只有 HTTP 一条路"
// 这条唯一业务通道。迁到此处后 CLI 只发 HTTP 请求。
//
// 重启是必须的，不是可选优化：轮换完成时，本进程内除 h.Vault 对应的这份 provider
// repo 外，还有其他独立构造的 credential.Vault 实例（如 Notion token 解密路径、
// ReloadProviders 用的 sb.Vault）持有同一把旧 masterKey 的内存副本——只重开一个 DB
// 连接改写密文，不会让它们同步换钥，继续用旧钥解密新密文必然失败。逐个热替换这些
// 消费点复杂且容易漏（新增消费点没有强制登记机制），进程重启后从新 vault.key 统一
// 重新加载是唯一能保证不留不一致状态的做法（当前阶段无生产用户，可接受重启代价）。
//
// 轮换写入 DB 到进程重启完成之间有一个短暂窗口：h.Vault（旧钥）仍在被其他并发请求
// 用于加解密，而 DB 里已是新钥密文，该窗口内的 provider 解密会失败。重启异步延迟
// 仅用于让本次 HTTP 响应有机会写出，窗口本身不可消除，由重启收敛，可接受。
func (h *SysAdminHandler) HandleVaultRotateMasterKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	authCtx := authcontext.FromContext(ctx)
	if !authCtx.Authenticated {
		httputil.RespondError(w, "authentication required", apperr.New(apperr.CodeForbidden, "vault rotate requires authentication"), http.StatusForbidden)
		return
	}
	if h.RWDB == nil || h.Vault == nil || h.DataDir == "" {
		httputil.RespondError(w, "vault is not configured", apperr.New(apperr.CodeInternal, "vault is not configured"), http.StatusInternalServerError)
		return
	}

	keyPath := filepath.Join(h.DataDir, "vault.key")
	newKeyPath := keyPath + ".new"

	newKey, err := credential.GenerateNewKey(newKeyPath)
	if err != nil {
		httputil.RespondError(w, "generate new key failed", err, http.StatusInternalServerError)
		return
	}
	newVault, err := credential.NewVaultWithKey(newKey)
	if err != nil {
		os.Remove(newKeyPath) //nolint:errcheck // 生成新 vault 失败，清理半成品密钥文件
		httputil.RespondError(w, "load new key failed", err, http.StatusInternalServerError)
		return
	}

	oldRepo := storerepo.NewSQLiteProviderRepository(h.RWDB).WithVault(h.Vault)
	newRepo := storerepo.NewSQLiteProviderRepository(h.RWDB).WithVault(newVault)

	providers, err := oldRepo.ListProviders(ctx)
	if err != nil {
		os.Remove(newKeyPath) //nolint:errcheck // 轮换失败回滚，清理半成品密钥文件
		httputil.RespondError(w, "list providers failed", err, http.StatusInternalServerError)
		return
	}

	rotated := 0
	for _, p := range providers {
		if p.APIKey == "" {
			continue
		}
		if err := newRepo.UpdateProviderAPIKey(ctx, p.ID, p.APIKey, p.UpdatedAt); err != nil {
			os.Remove(newKeyPath) //nolint:errcheck // 轮换中途失败，旧 vault.key 仍是当前密文的真实密钥，不落地半成品
			httputil.RespondError(w, "rotate provider "+p.ID+" failed", err, http.StatusInternalServerError)
			return
		}
		rotated++
	}

	if err := os.Rename(newKeyPath, keyPath); err != nil {
		httputil.RespondError(w, "swap key file failed", err, http.StatusInternalServerError)
		return
	}

	slog.Info("vault master key rotated", "actor", authCtx.UserID, "providers_rotated", rotated)
	httputil.WriteJSON(w, map[string]any{
		"status":            "rotated",
		"providers_rotated": rotated,
		"restarting":        h.Updater != nil,
	})

	if h.Updater != nil {
		go func() {
			time.Sleep(300 * time.Millisecond) // 让本次 HTTP 响应先写出，再退出/重启进程
			h.Updater.Restart()
		}()
	}
}
