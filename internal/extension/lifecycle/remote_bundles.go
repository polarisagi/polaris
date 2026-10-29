package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
)

// RemoteDownloader 远程文件下载（实现为 marketplace.SourceFetcher：仅 https、经 SafeDialer、有大小上限）。
type RemoteDownloader interface {
	DownloadTo(ctx context.Context, rawURL, destPath string) error
}

// remoteBundleDir 远程 .mcpb 包在插件目录内的落点（随插件目录一并卸载）。
const remoteBundleDir = ".polaris-remote-bundles"

// WithRemoteDownloader 注入远程 MCP 包下载器；未注入时清单中的 https .mcpb 引用记为诊断错误。
func (p *PluginInstaller) WithRemoteDownloader(d RemoteDownloader) *PluginInstaller {
	p.remote = d
	return p
}

// fetchRemoteBundles 预先下载插件清单 mcpServers 中的 https .mcpb/.dxt 包（解析器本身不发网络请求，
// ADR-0103 决策二）。单个包失败只影响该服务器：解析器会对未取回的引用报诊断。
func (p *PluginInstaller) fetchRemoteBundles(ctx context.Context, root string) map[string]string {
	urls := pluginspec.ListRemoteBundles(root)
	if len(urls) == 0 || p.remote == nil {
		return nil
	}
	out := make(map[string]string, len(urls))
	for _, u := range urls {
		sum := sha256.Sum256([]byte(u))
		dest := filepath.Join(root, remoteBundleDir, hex.EncodeToString(sum[:8])+filepath.Ext(u))
		if err := p.remote.DownloadTo(ctx, u, dest); err != nil {
			slog.Warn("plugin_installer: remote mcp bundle download failed", "url", u, "err", err)
			continue
		}
		out[u] = dest
	}
	return out
}
