package downloader

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// proxyHosts 内置按优先级排列的 GitHub 镜像站。
var proxyHosts = []string{
	"https://ghproxy.net",
	"https://mirror.ghproxy.com",
}

// ── 配置层（启动时由 Configure 写入，首次下载前必须完成） ──────────────────────

var cfgMu sync.RWMutex
var cfgValue string // 保存 Configure 传入的原始字符串，"" 表示尚未调用 Configure

// Configure 在进程启动时调用，设置 GitHub 下载代理策略。
// 必须在首次下载操作（ResolveURL / GitCloneOrPull）之前调用，否则退化为自动探测。
//
// proxyURL 取值：
//
//	"auto"                  — 自动探测（默认；不调用 Configure 时也是此行为）
//	"off" / "none"          — 禁用代理，始终直连 GitHub
//	"https://ghproxy.net"   — 强制使用指定代理，不再探测
//	"https://..."（任意 URL）— 同上，使用自定义代理前缀
//
// 环境变量 POLARIS_GITHUB_PROXY 优先级高于此函数，格式相同。
// 若环境变量已设置，Configure 的参数被忽略。
func Configure(proxyURL string) {
	if env := os.Getenv("POLARIS_GITHUB_PROXY"); env != "" {
		proxyURL = env
	}
	cfgMu.Lock()
	cfgValue = proxyURL
	cfgMu.Unlock()
	slog.Info("downloader: proxy configured", "value", maskURL(proxyURL))
}

// ── 运行时探测层（once 保证单次执行） ──────────────────────────────────────────

var probeOnce sync.Once
var resolvedProxy string // 最终生效的代理前缀；"" 表示直连

// probe 根据配置或自动探测决定 resolvedProxy，进程内只执行一次。
func probe(ctx context.Context, client *http.Client) {
	probeOnce.Do(func() {
		// 1. 环境变量最高优先（Configure 内已将 env 折叠进 cfgValue）
		cfgMu.RLock()
		val := cfgValue
		cfgMu.RUnlock()

		// 2. 若未调用 Configure，直接读环境变量
		if val == "" {
			val = os.Getenv("POLARIS_GITHUB_PROXY")
		}

		switch val {
		case "off", "none":
			// 强制直连，不探测
			slog.Info("downloader: GitHub proxy disabled by config")
			return
		case "", "auto":
			// 自动探测
			resolvedProxy = autoProbe(ctx, client)
		default:
			// 强制使用指定代理
			resolvedProxy = val
			slog.Info("downloader: GitHub proxy forced", "proxy", val)
		}
	})
}

// autoProbe 以 500ms 严格超时探测 github.com 区分网络环境：
//   - 海外 / VPN：github.com 通常 < 200ms → 直连，不需要代理
//   - 中国大陆：github.com 通常 ≥ 1s 或超时 → 切换镜像代理
//
// 即使中国大陆偶尔能连上 github.com，也因超时而正确切代理，避免不稳定下载。
func autoProbe(ctx context.Context, client *http.Client) string {
	// 500ms 内可达 → 海外/VPN 环境，直连即可
	fastClient := &http.Client{Timeout: 500 * time.Millisecond}
	if headOK(ctx, fastClient, "https://github.com") {
		slog.Info("downloader: GitHub reachable directly (<500ms), proxy not needed")
		return ""
	}

	// github.com 慢或不可达 → 判定为中国大陆或受限网络，尝试镜像代理
	slog.Info("downloader: GitHub slow/unreachable, probing China mainland proxy mirrors")
	slowClient := client
	if slowClient == nil {
		slowClient = &http.Client{Timeout: 6 * time.Second}
	}
	for _, p := range proxyHosts {
		if headOK(ctx, slowClient, p) {
			slog.Info("downloader: using GitHub proxy", "proxy", p)
			return p
		}
	}

	slog.Warn("downloader: all proxies unreachable, falling back to direct")
	return ""
}

// headOK 发起 HEAD 请求，有响应则返回 true。
func headOK(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// ── 公开 URL 解析接口 ──────────────────────────────────────────────────────────

// ResolveURL 将 github.com URL 解析为最优地址（直连或代理加速）。
// 结果由进程内探测/配置缓存决定，多次调用无额外网络开销。
func ResolveURL(ctx context.Context, client *http.Client, rawURL string) string {
	probe(ctx, client)
	if resolvedProxy == "" {
		return rawURL
	}
	return resolvedProxy + "/" + rawURL
}

// CandidateURLs 返回给定 URL 的全部候选地址（优先级排序）。
// 调用方可逐一尝试直到成功，适合需要多源降级的场景。
func CandidateURLs(ctx context.Context, client *http.Client, rawURL string) []string {
	probe(ctx, client)
	candidates := make([]string, 0, len(proxyHosts)+1)
	if resolvedProxy == "" {
		candidates = append(candidates, rawURL)
		for _, p := range proxyHosts {
			candidates = append(candidates, p+"/"+rawURL)
		}
	} else {
		candidates = append(candidates, resolvedProxy+"/"+rawURL)
		for _, p := range proxyHosts {
			if p != resolvedProxy {
				candidates = append(candidates, p+"/"+rawURL)
			}
		}
		candidates = append(candidates, rawURL)
	}
	return candidates
}

// ProxyStatus 返回当前生效的代理策略（仅在 probe 执行后准确）。
// 供 /health 或 debug 接口使用。
func ProxyStatus() string {
	if resolvedProxy == "" {
		return "direct"
	}
	return "proxy:" + resolvedProxy
}

func maskURL(u string) string {
	if u == "" {
		return "(auto)"
	}
	return u
}
