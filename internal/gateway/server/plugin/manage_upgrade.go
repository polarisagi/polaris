package plugin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HandleUpgradePlugin 处理插件升级请求 (B3, ADR-0075)。
//
// 复核修正（本轮审查）：初版实现只更新 installed_version 字符串，从未真正同步
// install_path 下的文件——对 ext_type='skill'/'plugin'（唯一真正落盘文件的两种
// 类型，见 020_extension_instances.sql "MCP/App 为空字符串" 注释）而言，这会让
// DB 记录的版本号与磁盘实际内容永久失真，且后续升级请求会因版本号"已匹配"而
// 被误判为无需处理，掩盖问题而非解决。现改为对 skill/plugin 复用安装期已验证
// 的 downloadAndInstallExtension（与 catalog_download.go 的全新安装路径同一份
// 实现，destDir 按 extID 确定性推导，天然幂等覆盖同一 install_path，不新建实例
// 行），同步执行以便在返回响应前拿到成功/失败结果；mcp/app 无落盘文件，版本号
// 同步即完整，维持原地更新。
//
// 拆分说明：本函数原与 manage.go 中其余 handler 同文件，因加入本实现后
// manage.go 超过 R7 400 行上限，按职责（升级流程自成一段）拆出独立文件，
// 不改变任何行为。
//
//nolint:gocyclo
func (h *PluginHandler) HandleUpgradePlugin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pluginID := r.PathValue("id")
	if pluginID == "" {
		http.Error(w, "plugin id required", http.StatusBadRequest)
		return
	}

	var installedVersion, catalogID, catalogVersion, extType, name string
	err := h.DB.QueryRowContext(ctx,
		`SELECT i.installed_version, i.catalog_id, COALESCE(c.version,''), i.ext_type, i.name
		 FROM extension_instances i
		 LEFT JOIN extension_catalog c ON i.catalog_id = c.id
		 WHERE i.id=?`, pluginID).Scan(&installedVersion, &catalogID, &catalogVersion, &extType, &name)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "plugin not found", http.StatusNotFound)
			return
		}
		httputil.RespondError(w, "query extension instance failed", apperr.Wrap(apperr.CodeInternal, "HandleUpgradePlugin", err), http.StatusInternalServerError)
		return
	}

	if catalogID == "" || catalogVersion == "" {
		http.Error(w, "该扩展不支持在线升级检测", http.StatusBadRequest)
		return
	}

	if installedVersion == catalogVersion {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotModified)
		_, _ = w.Write([]byte(`{"error": "already up to date"}`))
		return
	}

	newVersion := catalogVersion
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	if extType == "skill" || extType == "plugin" {
		// 落盘类型：必须真正同步文件，否则不得推进 installed_version。
		var payload string
		if scanErr := h.DB.QueryRowContext(ctx, `SELECT payload FROM extension_catalog WHERE id=?`, catalogID).Scan(&payload); scanErr != nil {
			httputil.RespondError(w, "catalog entry not found", apperr.Wrap(apperr.CodeInternal, "HandleUpgradePlugin", scanErr), http.StatusInternalServerError)
			return
		}
		var entry protocol.RegistryEntry
		if unmarshalErr := json.Unmarshal([]byte(payload), &entry); unmarshalErr != nil {
			httputil.RespondError(w, "malformed catalog entry", apperr.Wrap(apperr.CodeInvalidInput, "HandleUpgradePlugin", unmarshalErr), http.StatusInternalServerError)
			return
		}

		// 同步执行（而非 SafeGo 异步）：升级请求需要在响应前确认文件是否真正同步成功，
		// install_path 由 destDir 确定性推导（filepath.Join(DataDir,"extensions",extID)），
		// 与安装期完全一致，原地覆盖，不清空/不新建。
		h.downloadAndInstallExtension(ctx, pluginID, catalogID, &entry, now, name)

		var status, errMsg string
		if statusErr := h.DB.QueryRowContext(ctx, `SELECT status, error_msg FROM extension_instances WHERE id=?`, pluginID).Scan(&status, &errMsg); statusErr != nil {
			httputil.RespondError(w, "failed to verify upgrade result", apperr.Wrap(apperr.CodeInternal, "HandleUpgradePlugin", statusErr), http.StatusInternalServerError)
			return
		}
		if status == "error" {
			// downloadAndInstallExtension 内部已写入 error_msg，install_path 未被触碰
			// （updateExtensionInstanceError 只更新 status/error_msg 两列）。
			http.Error(w, "文件同步失败，install_path 保持原样: "+errMsg, http.StatusInternalServerError)
			return
		}
	}

	_, err = h.DB.ExecContext(ctx,
		"UPDATE extension_instances SET installed_version=?, status='installed', updated_at=? WHERE id=?",
		newVersion, now, pluginID,
	)
	if err != nil {
		_, _ = h.DB.ExecContext(ctx, "UPDATE extension_instances SET status='error', error_msg=?, updated_at=? WHERE id=?", err.Error(), now, pluginID)
		http.Error(w, "Failed to update plugin version", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "upgraded",
		"plugin_id":   pluginID,
		"old_version": installedVersion,
		"new_version": newVersion,
	})
}
