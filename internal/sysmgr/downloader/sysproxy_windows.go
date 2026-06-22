//go:build windows

package downloader

import (
	"log/slog"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

// detectAndInjectSystemProxy 读取 Windows 注册表中的 Internet 代理配置，
// 在未设置 HTTP_PROXY/HTTPS_PROXY 时自动为当前进程注入。
// 返回 true 表示检测到代理（已存在或刚注入），false 表示无代理。
func detectAndInjectSystemProxy() bool {
	if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" ||
		os.Getenv("http_proxy") != "" || os.Getenv("https_proxy") != "" {
		slog.Debug("downloader: proxy already configured via environment variables")
		return true
	}
	return injectFromRegistry()
}

// injectFromRegistry 从 HKCU Internet Settings 读取代理配置。
// 返回 true 表示成功注入了代理，false 表示无代理或读取失败。
//
// 注册表字段：
//   - ProxyEnable  DWORD  1=已启用
//   - ProxyServer  String 例如 "127.0.0.1:7890" 或 "http=127.0.0.1:7890;https=127.0.0.1:7890"
func injectFromRegistry() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		// 也尝试 HKLM（企业 GPO 场景）
		k, err = registry.OpenKey(registry.LOCAL_MACHINE, inetSettingsKey, registry.QUERY_VALUE)
		if err != nil {
			slog.Debug("downloader: windows registry proxy key not found", "err", err)
			return false
		}
	}
	defer k.Close()

	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		slog.Debug("downloader: windows proxy not enabled")
		return false
	}

	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		slog.Debug("downloader: windows ProxyServer empty")
		return false
	}

	// server 可能是 "host:port" 或 "http=host:port;https=host:port;..."
	httpProxy, httpsProxy := parseWindowsProxyServer(server)

	injected := false
	if httpsProxy != "" {
		os.Setenv("HTTPS_PROXY", "http://"+httpsProxy) //nolint:errcheck
		slog.Info("downloader: injected HTTPS_PROXY from Windows registry", "proxy", httpsProxy)
		injected = true
	}
	if httpProxy != "" {
		os.Setenv("HTTP_PROXY", "http://"+httpProxy) //nolint:errcheck
		slog.Info("downloader: injected HTTP_PROXY from Windows registry", "proxy", httpProxy)
		injected = true
	}
	return injected
}

// parseWindowsProxyServer 解析 Windows 的 ProxyServer 字符串。
// 支持两种格式：
//   - "127.0.0.1:7890"（全局统一代理）
//   - "http=127.0.0.1:7890;https=127.0.0.1:7890;ftp=..."（分协议代理）
func parseWindowsProxyServer(s string) (httpProxy, httpsProxy string) {
	if !strings.Contains(s, "=") {
		// 全局统一代理格式
		return s, s
	}
	for _, part := range strings.Split(s, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(kv[0]))
		addr := strings.TrimSpace(kv[1])
		switch proto {
		case "http":
			httpProxy = addr
		case "https":
			httpsProxy = addr
		}
	}
	// https 未单独设置时，降级用 http 代理
	if httpsProxy == "" {
		httpsProxy = httpProxy
	}
	return
}
