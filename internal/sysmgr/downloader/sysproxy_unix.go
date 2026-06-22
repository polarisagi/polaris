//go:build !windows

package downloader

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// detectAndInjectSystemProxy 读取操作系统级别的代理配置。
//
// 若检测到系统代理（HTTP/HTTPS/SOCKS5）且当前进程尚未设置 HTTP_PROXY/HTTPS_PROXY，
// 则自动为当前进程注入环境变量，使 Go 的 http.DefaultTransport.ProxyFromEnvironment 生效，
// 并返回 true（表示"已有系统代理，无需进一步镜像探测"）。
//
// 若当前进程已有代理环境变量（用户手动设置），同样返回 true。
// 若无任何代理，返回 false。
//
// 支持平台：
//   - macOS：通过 scutil --proxy 读取系统网络配置
//   - Linux：读取标准 http_proxy / https_proxy 环境变量（已有则直接生效）
func detectAndInjectSystemProxy() bool {
	// 已有环境变量，用户已手动设置，直接认为有代理
	if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" ||
		os.Getenv("http_proxy") != "" || os.Getenv("https_proxy") != "" {
		slog.Debug("downloader: proxy already configured via environment variables")
		return true
	}

	switch runtime.GOOS {
	case "darwin":
		return injectFromScutil()
	case "linux":
		// Linux 标准环境变量已由上面检查覆盖。
		// 部分桌面环境（GNOME）使用 gsettings，但差异太大，不做通用处理。
		slog.Debug("downloader: linux proxy detection relies on standard env vars")
		return false
	default:
		return false
	}
}

// injectFromScutil 解析 macOS 的 scutil --proxy 输出并注入代理环境变量。
// 返回 true 表示成功注入了至少一个代理。
//
// scutil --proxy 典型输出：
//
//	<dictionary> {
//	  HTTPEnable : 1
//	  HTTPProxy : 127.0.0.1
//	  HTTPPort : 7890
//	  HTTPSEnable : 1
//	  HTTPSProxy : 127.0.0.1
//	  HTTPSPort : 7890
//	}
func injectFromScutil() bool {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		slog.Debug("downloader: scutil --proxy failed", "err", err)
		return false
	}
	kv := parseScutilOutput(out)

	injected := false

	httpsEnabled := kv["HTTPSEnable"] == "1"
	httpsHost := kv["HTTPSProxy"]
	httpsPort := kv["HTTPSPort"]
	if httpsEnabled && httpsHost != "" && httpsPort != "" {
		proxyURL := "http://" + httpsHost + ":" + httpsPort
		os.Setenv("HTTPS_PROXY", proxyURL) //nolint:errcheck
		slog.Info("downloader: injected HTTPS_PROXY from macOS system settings", "proxy", proxyURL)
		injected = true
	}

	httpEnabled := kv["HTTPEnable"] == "1"
	httpHost := kv["HTTPProxy"]
	httpPort := kv["HTTPPort"]
	if httpEnabled && httpHost != "" && httpPort != "" {
		proxyURL := "http://" + httpHost + ":" + httpPort
		os.Setenv("HTTP_PROXY", proxyURL) //nolint:errcheck
		slog.Info("downloader: injected HTTP_PROXY from macOS system settings", "proxy", proxyURL)
		injected = true
	}

	// SOCKS5 代理（Clash 等工具常用）
	socksEnabled := kv["SOCKSEnable"] == "1"
	socksHost := kv["SOCKSProxy"]
	socksPort := kv["SOCKSPort"]
	if socksEnabled && socksHost != "" && socksPort != "" {
		if os.Getenv("HTTPS_PROXY") == "" {
			proxyURL := "socks5://" + socksHost + ":" + socksPort
			os.Setenv("HTTPS_PROXY", proxyURL) //nolint:errcheck
			os.Setenv("HTTP_PROXY", proxyURL)  //nolint:errcheck
			slog.Info("downloader: injected SOCKS5 proxy from macOS system settings", "proxy", proxyURL)
			injected = true
		}
	}

	return injected
}

// parseScutilOutput 将 scutil --proxy 的文本输出解析为 key→value 映射。
func parseScutilOutput(data []byte) map[string]string {
	result := make(map[string]string)
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		// 格式：  Key : Value
		idx := bytes.Index(line, []byte(" : "))
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(string(line[:idx]))
		val := strings.TrimSpace(string(line[idx+3:]))
		if key != "" && val != "" {
			result[key] = val
		}
	}
	return result
}
