package downloader

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// proxyMirrors 返回内置的 GitHub 镜像站列表（按优先级排序）。
// Go 不支持 []string const，使用函数替代包级 var 切片，保持不可变语义。
func proxyMirrors() []string {
	return []string{
		"https://ghproxy.net",
		"https://ghp.ci",
		"https://gh-proxy.com",
		"https://mirror.ghproxy.com",
	}
}

// proxyState 封装代理配置和探测结果的所有可变状态。
// 通过 getProxy() 单例访问，消除 6 个包级可变变量。
type proxyState struct {
	cfgMu         sync.RWMutex
	cfgValue      string       // Configure 传入的原始策略字符串，"" 表示尚未配置
	cfgHTTPClient *http.Client // 注入的安全客户端；nil 时退化为带超时的裸 client
	probeOnce     sync.Once
	resolved      string // 最终生效的代理前缀；"" 表示直连
}

// getProxy 返回进程级 proxyState 单例（sync.OnceValue 惰性初始化，线程安全）。
var getProxy = sync.OnceValue(func() *proxyState { return &proxyState{} })

// Configure 在进程启动时调用，设置 GitHub 下载代理策略。
// httpClient 应传入经 SafeDialer 包装的客户端（遵循 XR-06）；传 nil 使用默认 transport。
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
func Configure(proxyURL string, httpClient *http.Client) {
	if env := os.Getenv("POLARIS_GITHUB_PROXY"); env != "" {
		proxyURL = env
	}
	s := getProxy()
	s.cfgMu.Lock()
	s.cfgValue = proxyURL
	s.cfgHTTPClient = httpClient
	s.cfgMu.Unlock()
	slog.Info("downloader: proxy configured", "value", maskURL(proxyURL))
}

// ── 运行时探测层（probeOnce 保证单次执行） ──────────────────────────────────────

// probe 根据配置或自动探测决定 resolved proxy，进程内只执行一次。
func probe(ctx context.Context, _ *http.Client) {
	s := getProxy()
	s.probeOnce.Do(func() {
		// Step 0: 读取操作系统级系统代理。
		// 若检测到系统代理（Clash、V2Ray、VPN、卡巴斯基等），直接跳过后续探测：
		//   - 系统代理本身负责 GitHub 流量路由，无需我们的镜像机制叠加干预。
		//   - GeoIP 探测用的 SafeDialer transport 不走系统代理，继续探测必然误判。
		// resolved 保持 "" 即"直连"，系统代理在 OS 层面透明接管。
		if detectAndInjectSystemProxy() {
			slog.Info("downloader: system proxy detected, skipping GeoIP probe and mirror selection")
			return
		}

		s.cfgMu.RLock()
		val := s.cfgValue
		safeClient := s.cfgHTTPClient
		s.cfgMu.RUnlock()

		if val == "" {
			val = os.Getenv("POLARIS_GITHUB_PROXY")
		}

		// 探测用 client：优先注入的 safeClient，降级带超时裸 client
		probeClient := safeClient
		if probeClient == nil {
			probeClient = &http.Client{Timeout: 6 * time.Second}
		}

		switch val {
		case "off", "none":
			slog.Info("downloader: GitHub proxy disabled by config")
			return
		case "", "auto":
			s.resolved = autoProbe(ctx, probeClient)
		default:
			// 强制使用指定代理
			s.resolved = val
			slog.Info("downloader: GitHub proxy forced", "proxy", val)
		}
	})
}

// autoProbe 探测当前网络环境以决定是否使用 GitHub 镜像代理。
//
// 决策逻辑：直接测试 GitHub 连通性（而非 GeoIP 国家判断）。
//   - 能连上 GitHub → 直连（海外/VPN/TUN 模式/系统 HTTP 代理均在此命中）
//   - 连不上 GitHub → 并发竞速所有内置镜像，取最快响应者
//
// 注：GeoIP 探测（isMainlandChina）已从此路径中移除，
// 将在未来由独立的用户画像/本地化模块（sysmgr/locale）承接。
func autoProbe(ctx context.Context, _ *http.Client) string {
	if canReachGitHub(ctx) {
		slog.Info("downloader: GitHub reachable directly, proxy not needed")
		return ""
	}

	slog.Info("downloader: GitHub unreachable, racing proxy mirrors")
	if winner := raceFastestMirror(ctx, nil); winner != "" {
		slog.Info("downloader: using GitHub proxy (won race)", "proxy", winner)
		return winner
	}

	slog.Warn("downloader: all proxies unreachable, falling back to direct")
	return ""
}

// canReachGitHub 用 http.DefaultTransport 测试能否直连 GitHub。
//
// 刻意不使用 SafeDialer transport，原因：
//   - SafeDialer 有自己的 DialContext，不走系统 TUN 隧道
//   - SafeDialer 不读 HTTPS_PROXY 环境变量
//   - http.DefaultTransport 能透明利用 TUN 模式和 HTTPS_PROXY（detectAndInjectSystemProxy 注入）
//
// github.com 是公共外部域名，不存在 SSRF 风险，无需 SafeDialer 保护。
func canReachGitHub(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: http.DefaultTransport,
	}
	return headOK(probeCtx, client, "https://github.com")
}

// raceFastestMirror 并发向所有内置镜像站发起 HEAD 请求，返回最先成功响应的镜像 URL。
// 若全部超时或失败，返回空字符串（降级直连）。
// 并发竞速相比串行逐一探测，总耗时 = max(单次延迟) 而非 sum(单次延迟)。
func raceFastestMirror(ctx context.Context, baseClient *http.Client) string {
	mirrors := proxyMirrors()
	raceCtx, raceCancel := context.WithTimeout(ctx, 6*time.Second)
	defer raceCancel()

	type result struct{ mirror string }
	winCh := make(chan result, len(mirrors))

	var transport http.RoundTripper = http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	raceClient := &http.Client{
		Timeout:   6 * time.Second,
		Transport: transport,
	}

	for _, m := range mirrors {
		m := m
		go func() {
			if headOK(raceCtx, raceClient, m) {
				select {
				case winCh <- result{m}:
				case <-raceCtx.Done():
				}
			}
		}()
	}

	select {
	case w := <-winCh:
		return w.mirror
	case <-raceCtx.Done():
		return ""
	}
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
func ResolveURL(ctx context.Context, _ *http.Client, rawURL string) string {
	probe(ctx, nil)
	if getProxy().resolved == "" {
		return rawURL
	}
	return getProxy().resolved + "/" + rawURL
}

// CandidateURLs 返回给定 URL 的全部候选地址（优先级排序）。
// 调用方可逐一尝试直到成功，适合需要多源降级的场景。
func CandidateURLs(ctx context.Context, _ *http.Client, rawURL string) []string {
	probe(ctx, nil)
	mirrors := proxyMirrors()
	resolved := getProxy().resolved
	candidates := make([]string, 0, len(mirrors)+1)
	if resolved == "" {
		candidates = append(candidates, rawURL)
		for _, p := range mirrors {
			candidates = append(candidates, p+"/"+rawURL)
		}
	} else {
		candidates = append(candidates, resolved+"/"+rawURL)
		for _, p := range mirrors {
			if p != resolved {
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
	if getProxy().resolved == "" {
		return "direct"
	}
	return "proxy:" + getProxy().resolved
}

func maskURL(u string) string {
	if u == "" {
		return "(auto)"
	}
	idx := strings.Index(u, "://")
	if idx == -1 {
		return u
	}
	atIdx := strings.Index(u[idx+3:], "@")
	if atIdx == -1 {
		return u
	}
	colonIdx := strings.Index(u[idx+3:idx+3+atIdx], ":")
	if colonIdx == -1 {
		return u
	}

	return u[:idx+3+colonIdx+1] + "***" + u[idx+3+atIdx:]
}
