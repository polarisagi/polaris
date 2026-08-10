package mcp

import (
	"net/http"

	"github.com/polarisagi/polaris/internal/security/network"
)

// testSafeHTTP 构造测试用的 network.SafeHTTPClient。
//
// 2026-08-10：NewMCPManager/NewMCPClient 的 httpClient 形参从裸 *http.Client 收窄为
// network.SafeHTTPClient（XR-06 出站网络禁裸 client）。测试此前直接传
// http.DefaultClient / nil / 自建 &http.Client{Transport: mock}，这三种写法本身就是
// 该收窄要拦的形态——能编过恰恰说明此前没有任何类型级约束。
//
// 保留 mock Transport 注入能力的做法：先用官方工厂拿到带 isSafe 标记的实例，
// 再替换其 Transport。这样测试仍能拦截请求，而"必须经工厂构造"这条约束不被绕过。
func testSafeHTTP(rt http.RoundTripper) network.SafeHTTPClient {
	c := network.NewSafeHTTPClient(nil)
	if rt != nil {
		c.Transport = rt
	}
	return c
}
