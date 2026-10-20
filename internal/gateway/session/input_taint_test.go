package session

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// 输入污点：owner 本人经本地可信客户端 -> Low；外部渠道/未鉴权 -> High，且不可被自报抬升。
func TestInputTaintLevel(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want types.TaintLevel
	}{
		{"web owner", Request{Channel: "web", OwnerTrusted: true}, types.TaintLow},
		{"cli owner", Request{Channel: "cli", OwnerTrusted: true}, types.TaintLow},
		{"web but not authenticated as owner", Request{Channel: "web"}, types.TaintHigh},
		{"telegram", Request{Channel: "telegram"}, types.TaintHigh},
		{"telegram with owner flag wrongly set", Request{Channel: "telegram", OwnerTrusted: true}, types.TaintHigh},
		{"webhook", Request{Channel: "webhook", OwnerTrusted: true}, types.TaintHigh},
		{"plugin channel", Request{Channel: "plugin_channel", OwnerTrusted: true}, types.TaintHigh},
		{"cron", Request{Channel: "cron", OwnerTrusted: true, Headless: true}, types.TaintHigh},
		{"empty channel", Request{OwnerTrusted: true}, types.TaintHigh},
	}
	for _, tc := range cases {
		if got := inputTaintLevel(tc.req); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
