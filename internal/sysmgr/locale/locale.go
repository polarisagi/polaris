// Package locale 提供用户地区与本地化信息探测能力。
//
// 探测策略（按优先级）：
//  1. 系统时区（毫秒级，无网络开销）—— 仅能判断大致地区，不精确到国家
//  2. GeoIP 并发探测（ipinfo.io / 1.1.1.1 / api.ip.sb）—— 网络依赖，结果最准确
//  3. 本地缓存（TODO: 写入 DB，避免每次启动都探测）
//
// 该包与下载代理决策完全解耦，供用户画像、内容个性化、语言偏好等功能调用。
package locale

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UserLocale 保存探测到的用户地区信息。
type UserLocale struct {
	// Country 是 ISO 3166-1 alpha-2 国家代码，例如 "CN"、"US"。
	// 若探测失败则为空字符串。
	Country string

	// TimeZone 是系统时区名，例如 "Asia/Shanghai"。
	// 始终由本地系统读取，不依赖网络。
	TimeZone string

	// Source 表示 Country 字段的数据来源：
	//   "geoip"    — 通过 GeoIP 服务探测得到
	//   "timezone" — GeoIP 失败，由时区推断（精度较低）
	//   "unknown"  — 无法判断
	Source string
}

// IsMainlandChina 返回该地区是否为中国大陆。
func (l *UserLocale) IsMainlandChina() bool {
	return l.Country == "CN"
}

// Detect 探测当前用户的地区信息。
//
// 探测过程：
//  1. 读取系统时区（同步，无网络）
//  2. 并发请求三个 GeoIP 服务，取最先成功的结果（最多等待 geoIPTimeout）
//  3. 若 GeoIP 全部失败，降级为根据时区推断国家
//
// httpClient 应传入调用方管理的 HTTP 客户端；传 nil 使用 http.DefaultClient。
// 注意：为避免 SafeDialer 的 SSRF 限制干扰外部公共 API 的访问，
// 建议传入基于 http.DefaultTransport 的客户端。
func Detect(ctx context.Context, httpClient *http.Client) *UserLocale {
	tz := readSystemTimezone()

	country, ok := probeGeoIP(ctx, httpClient)
	if ok {
		slog.Debug("locale: GeoIP detected", "country", country, "timezone", tz)
		return &UserLocale{Country: country, TimeZone: tz, Source: "geoip"}
	}

	slog.Warn("locale: GeoIP probes all failed, falling back to timezone inference", "timezone", tz)
	inferred := inferCountryFromTimezone(tz)
	if inferred != "" {
		return &UserLocale{Country: inferred, TimeZone: tz, Source: "timezone"}
	}

	return &UserLocale{Country: "", TimeZone: tz, Source: "unknown"}
}

// ── GeoIP 并发探测 ─────────────────────────────────────────────────────────────

const geoIPTimeout = 5 * time.Second

// probeGeoIP 并发向三个 GeoIP 服务发起请求，返回第一个成功结果。
// 若全部超时或失败，返回 ("", false)。
//
//nolint:gocyclo
func probeGeoIP(ctx context.Context, baseClient *http.Client) (country string, ok bool) {
	client := baseClient
	if client == nil {
		client = &http.Client{Timeout: geoIPTimeout}
	}

	probeCtx, cancel := context.WithTimeout(ctx, geoIPTimeout)
	defer cancel()

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
		if resp.StatusCode == http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			if c := strings.TrimSpace(string(b)); len(c) == 2 {
				resCh <- c
			}
		}
	}()

	// 探测 2: Cloudflare 1.1.1.1 trace
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
		if resp.StatusCode == http.StatusOK {
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
		if resp.StatusCode == http.StatusOK {
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

	for c := range resCh {
		if c != "" {
			return c, true
		}
	}
	return "", false
}

// ── 时区推断 ───────────────────────────────────────────────────────────────────

// readSystemTimezone 读取系统当前时区名（如 "Asia/Shanghai"）。
func readSystemTimezone() string {
	return time.Local.String()
}

// inferCountryFromTimezone 根据时区名推断最可能的国家代码。
// 仅覆盖常见时区，精度较低，仅作 GeoIP 失败时的兜底。
//
//nolint:gocyclo
func inferCountryFromTimezone(tz string) string {
	// 中国大陆：标准时区为 Asia/Shanghai 或 CST
	if strings.HasPrefix(tz, "Asia/Shanghai") ||
		strings.HasPrefix(tz, "Asia/Chongqing") ||
		strings.HasPrefix(tz, "Asia/Harbin") ||
		strings.HasPrefix(tz, "Asia/Urumqi") ||
		tz == "CST" {
		return "CN"
	}
	// 港澳台
	if tz == "Asia/Hong_Kong" {
		return "HK"
	}
	if tz == "Asia/Taipei" {
		return "TW"
	}
	if tz == "Asia/Macau" {
		return "MO"
	}
	// 日本
	if tz == "Asia/Tokyo" {
		return "JP"
	}
	// 韩国
	if tz == "Asia/Seoul" {
		return "KR"
	}
	// 美国（东、中、山、太平洋）
	if strings.HasPrefix(tz, "America/New_York") ||
		strings.HasPrefix(tz, "America/Chicago") ||
		strings.HasPrefix(tz, "America/Denver") ||
		strings.HasPrefix(tz, "America/Los_Angeles") {
		return "US"
	}
	// 英国
	if tz == "Europe/London" {
		return "GB"
	}
	// 德国 / 法国（CET）
	if tz == "Europe/Berlin" || tz == "Europe/Paris" {
		return "DE"
	}
	return ""
}
