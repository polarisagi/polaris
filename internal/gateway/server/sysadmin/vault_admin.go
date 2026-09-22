package sysadmin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// HandleVaultRotateMasterKey 轮换 credential.Vault 主密钥：实际的解密/重新加密/
// 原子替换 vault.key 由 h.RotateVaultMasterKey 完成（闭包由 server_lifecycle.go
// 的 newVaultMasterKeyRotator 构造，见该文件头注释）。本方法只负责鉴权、调用与
// 轮换成功后异步触发守护进程重启。
//
// 迁移背景（ADR-0096 决策一）：此前 `polaris vault rotate-master-key` 由
// cmd/polaris/cli_vault.go 直连 SQLite 完成，绕过"客户端与内核之间只有 HTTP 一条路"
// 这条唯一业务通道。迁到此处后 CLI 只发 HTTP 请求。
//
// 重启是必须的，不是可选优化：轮换写入 DB 到进程重启完成之间有一个短暂窗口，
// 期间进程内独立持有旧 vault 的其他消费点（Notion token、ReloadProviders 等）
// 仍会用旧 key 读写而 DB 里已是新密文，该窗口内的解密会失败；重启后统一从新
// vault.key 重新加载才能收敛到一致状态（当前阶段无生产用户，可接受重启代价）。
func (h *SysAdminHandler) HandleVaultRotateMasterKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	authCtx := authcontext.FromContext(ctx)
	if !authCtx.Authenticated {
		httputil.RespondError(w, "authentication required", apperr.New(apperr.CodeForbidden, "vault rotate requires authentication"), http.StatusForbidden)
		return
	}
	if h.RotateVaultMasterKey == nil {
		httputil.RespondError(w, "vault rotation is not configured", apperr.New(apperr.CodeInternal, "vault rotation is not configured"), http.StatusInternalServerError)
		return
	}

	rotated, err := h.RotateVaultMasterKey(ctx)
	if err != nil {
		httputil.RespondError(w, "vault rotate failed", err, http.StatusInternalServerError)
		return
	}

	slog.Info("vault master key rotated", "actor", authCtx.UserID, "providers_rotated", rotated)
	httputil.WriteJSON(w, map[string]any{
		"status":            "rotated",
		"providers_rotated": rotated,
		"restarting":        h.Updater != nil,
	})

	if h.Updater != nil {
		concurrent.SafeGo(context.Background(), "sysadmin.vault_rotate.restart", func(context.Context) {
			time.Sleep(300 * time.Millisecond) // 让本次 HTTP 响应先写出，再退出/重启进程
			h.Updater.Restart()
		})
	}
}
