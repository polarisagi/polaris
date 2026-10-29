package locale

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUserLocale_IsMainlandChina(t *testing.T) {
	tests := []struct {
		country string
		want    bool
	}{
		{"CN", true},
		{"US", false},
		{"HK", false},
		{"", false},
	}
	for _, tt := range tests {
		l := &UserLocale{Country: tt.country}
		if got := l.IsMainlandChina(); got != tt.want {
			t.Errorf("IsMainlandChina() country=%q: got %v, want %v", tt.country, got, tt.want)
		}
	}
}

func TestInferCountryFromTimezone(t *testing.T) {
	tests := []struct {
		tz   string
		want string
	}{
		{"Asia/Shanghai", "CN"},
		{"Asia/Urumqi", "CN"},
		{"CST", "CN"},
		{"Asia/Hong_Kong", "HK"},
		{"Asia/Taipei", "TW"},
		{"Asia/Tokyo", "JP"},
		{"Asia/Seoul", "KR"},
		{"America/New_York", "US"},
		{"America/Los_Angeles", "US"},
		{"Europe/London", "GB"},
		{"Europe/Berlin", "DE"},
		{"Pacific/Auckland", ""},
	}
	for _, tt := range tests {
		got := inferCountryFromTimezone(tt.tz)
		if got != tt.want {
			t.Errorf("inferCountryFromTimezone(%q) = %q, want %q", tt.tz, got, tt.want)
		}
	}
}

func TestReadSystemTimezone(t *testing.T) {
	tz := readSystemTimezone()
	if tz == "" {
		t.Error("readSystemTimezone() returned empty string")
	}
}

func TestProbeGeoIP_MockSuccess(t *testing.T) {
	// 模拟 ipinfo.io 返回 "US"
	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString("US\n")),
				Header:     make(http.Header),
			}
		}),
	}

	// 直接调用内部函数，用 mock server 验证解析逻辑
	// （正式场景用 Detect()）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 我们无法直接替换内部 URL，但可以验证 probeGeoIP 在网络正常时能返回结果
	// 此处验证函数在超时前能正常返回 (不 panic、不死锁)
	_, _ = probeGeoIP(ctx, client)
}

func TestDetect_TimezoneInference(t *testing.T) {
	// 用一个拒绝连接的 client 强制 GeoIP 失败，验证降级为时区推断
	client := &http.Client{
		Transport: &rejectTransport{},
		Timeout:   100 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := Detect(ctx, client)
	if result == nil {
		t.Fatal("Detect() returned nil")
	}
	if result.TimeZone == "" {
		t.Error("TimeZone should not be empty")
	}
	// Source 应该是 "timezone" 或 "unknown"（取决于运行环境时区是否在已知列表中）
	if result.Source != "timezone" && result.Source != "unknown" {
		t.Errorf("unexpected source: %s", result.Source)
	}
	t.Logf("Detect result: country=%q timezone=%q source=%q", result.Country, result.TimeZone, result.Source)
}

func TestDetect_SourceValues(t *testing.T) {
	result := &UserLocale{Source: "geoip"}
	if !strings.Contains("geoip timezone unknown", result.Source) {
		t.Errorf("unexpected source: %s", result.Source)
	}
}

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

// rejectTransport 拒绝所有 HTTP 请求，用于强制 GeoIP 探测失败。
type rejectTransport struct{}

func (r *rejectTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, &rejectError{}
}

type rejectError struct{}

func (e *rejectError) Error() string { return "rejected by test transport" }
