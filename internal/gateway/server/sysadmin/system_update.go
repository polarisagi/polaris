package sysadmin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"
)

// isLocalOrigin 检查 Origin 头是否来自本机浏览器访问。
// 更新操作属于特权操作，必须拒绝跨域浏览器请求（CSRF 防御）。
// API Key 客户端不设置 Origin 头，不受此检查影响。
//
// 使用 url.Parse + Hostname() 精确匹配，防止前缀子串绕过
// （如 "http://localhost.evil.com" 能骗过 strings.HasPrefix 但无法通过 hostname 精确比较）。
func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// Hostname() 去除端口号和 IPv6 方括号
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// requireLocalOrigin 当请求来自浏览器（Origin 头存在）且不是本机时，拒绝请求。
// 返回 false 表示已写入错误响应，调用方应立即 return。
func requireLocalOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" && !isLocalOrigin(origin) {
		http.Error(w, "403 Forbidden: cross-origin requests not allowed for update endpoints", http.StatusForbidden)
		return false
	}
	return true
}

// handleGetVersion 返回当前版本信息与正在进行的更新进度（前端轮询入口）。
// 版本对比由前端直接调 GitHub API 完成，后端只提供 current + update_status + update_error。
func (h *SysAdminHandler) HandleGetVersion(w http.ResponseWriter, _ *http.Request) {
	if h.Updater == nil {
		respondJSON(w, http.StatusOK, updater.VersionInfo{
			Current:      "dev",
			UpdateStatus: updater.StatusIdle,
		})
		return
	}
	respondJSON(w, http.StatusOK, h.Updater.GetVersionInfo())
}

// handleTriggerUpdate 启动下载 → SHA-256 校验 → 原子替换 → 重启流程。
// 请求体：{"version": "v1.2.3"}，版本由前端从 GitHub API 获取后传入。
func (h *SysAdminHandler) HandleTriggerUpdate(w http.ResponseWriter, r *http.Request) {
	// CSRF 防御：拒绝来自非 localhost 域名的浏览器跨域请求
	if !requireLocalOrigin(w, r) {
		return
	}
	if h.Updater == nil {
		http.Error(w, "updater not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version == "" {
		http.Error(w, "request body must contain {\"version\": \"v1.x.x\"}", http.StatusBadRequest)
		return
	}

	if err := h.Updater.TriggerUpdate(r.Context(), body.Version); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "update_started", "version": body.Version})
}

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
