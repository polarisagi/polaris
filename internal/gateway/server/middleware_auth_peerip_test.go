package server

import (
	"net/http/httptest"
	"testing"
)

// TestPeerIP_IgnoresForwardedHeaders 钉死：零认证模式下的回环判定只看 TCP 对端，
// 不看任何请求头。
//
// 立此用例的原因：extractIP 在 POLARIS_TRUSTED_PROXY=1 时会采信 X-Forwarded-For，
// 该开关的前提是「前面真有一个会重写该头的反代」。运营者开了开关却没有真反代，
// 或反代被绕过直连时，攻击者只需伪造 `X-Forwarded-For: 127.0.0.1` 就能让
// checkAuth 的 loopback 分支为真，在未设 POLARIS_API_KEY 的部署上直接拿到完整权限。
// 鉴权判定的输入必须不可被客户端影响——这条不能靠"记得别用 extractIP"来保证。
func TestPeerIP_IgnoresForwardedHeaders(t *testing.T) {
	t.Setenv("POLARIS_TRUSTED_PROXY", "1")

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		wantLocal  bool
	}{
		{"远程直连 + 伪造 XFF 回环", "203.0.113.9:51234", "127.0.0.1", false},
		{"远程直连 + 伪造 XFF IPv6 回环", "203.0.113.9:51234", "::1", false},
		{"远程直连 + 多跳伪造", "203.0.113.9:51234", "10.0.0.1, 127.0.0.1", false},
		{"真回环 IPv4", "127.0.0.1:51234", "", true},
		{"真回环 IPv6", "[::1]:51234", "", true},
		{"真回环但 XFF 声称远程", "127.0.0.1:51234", "203.0.113.9", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/chat", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}

			if got := isLoopback(peerIP(r)); got != tc.wantLocal {
				t.Errorf("isLoopback(peerIP(r)) = %v，期望 %v（RemoteAddr=%q XFF=%q）",
					got, tc.wantLocal, tc.remoteAddr, tc.xff)
			}

			// 对照：extractIP 会被 XFF 带偏——这正是它不能用于鉴权判定的证据。
			// 该断言同时锁住"两者分工不同"这件事，防止后来者把 peerIP 改回 extractIP。
			if tc.xff != "" && extractIP(r) == peerIP(r) {
				t.Errorf("extractIP 与 peerIP 在存在 XFF 时不应相同，两者分工被抹平了")
			}
		})
	}
}
