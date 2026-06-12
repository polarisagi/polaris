package downloader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
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

// autoProbe 探测当前网络环境以决定是否使用代理。
//   - 海外 / VPN：直接连接
//   - 中国大陆：使用镜像代理
func autoProbe(ctx context.Context, client *http.Client) string {
	if !isMainlandChina(ctx, client) {
		slog.Info("downloader: Network is outside mainland China or VPN active, proxy not needed")
		return ""
	}

	slog.Info("downloader: Mainland China network detected, probing proxy mirrors")
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

// isMainlandChina 并发探测多个 GeoIP 服务以判断用户是否在中国大陆。
// 如果用户挂了全局 VPN，出口 IP 为海外，将会返回 false。
// 探测过程带有 2 秒超时，如果全失败，则降级为测试 GitHub 的连通性。
//
//nolint:gocyclo
func isMainlandChina(ctx context.Context, baseClient *http.Client) bool {
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()

	var transport http.RoundTripper = http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
	}

	resCh := make(chan string, 3)
	var wg sync.WaitGroup

	// 探测 1: ipinfo.io
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://ipinfo.io/country", nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "curl/8.0.0")
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			if country := strings.TrimSpace(string(b)); len(country) == 2 {
				resCh <- country
			}
		}
	}()

	// 探测 2: 1.1.1.1 trace
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://1.1.1.1/cdn-cgi/trace", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "loc=") {
					resCh <- strings.TrimSpace(strings.TrimPrefix(line, "loc="))
					return
				}
			}
		}
	}()

	// 探测 3: api.ip.sb
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://api.ip.sb/geoip", nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", "curl/8.0.0")
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var data struct {
				CountryCode string `json:"country_code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err == nil && data.CountryCode != "" {
				resCh <- data.CountryCode
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resCh)
	}()

	// 获取第一个成功的国家代码
	for country := range resCh {
		if country != "" {
			isCN := (country == "CN")
			slog.Debug("downloader: GeoIP resolved", "country", country, "is_cn", isCN)
			return isCN
		}
	}

	slog.Warn("downloader: all GeoIP probes failed, falling back to GitHub ping")
	// 降级: 测试 GitHub 连通性
	fastClient := &http.Client{
		Timeout:   1 * time.Second,
		Transport: transport,
	}
	if headOK(ctx, fastClient, "https://github.com") {
		return false // 能快速连上，假设为非大陆或已代理
	}
	return true // 连不上，假设为大陆
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
