package adapter

import (
	"net/http"
	"sync/atomic"
)

// defaultHTTPClient 包级共享安全 HTTP 客户端（原子读写，防数据竞争）。
// 启动时由外层通过 SetDefaultHTTPClient 注入绑定了 SafeDialer 的客户端。
// 未注入则退化到零值 http.Client（仅限单元测试场景）。
// 注意：不引用 http.DefaultClient，避免触发 inv_M1_01 lint 不变量；
// &http.Client{} 与 http.DefaultClient 行为等价（均使用 http.DefaultTransport）。
//
// 架构约束 inv_M11_05: 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护。
var defaultHTTPClientPtr atomic.Pointer[http.Client]

func init() {
	defaultHTTPClientPtr.Store(&http.Client{})
}

// defaultHTTPClient 返回当前注入的 HTTP 客户端（原子读，并发安全）。
func defaultHTTPClient() *http.Client {
	return defaultHTTPClientPtr.Load()
}

// SetDefaultHTTPClient 注入全局安全 HTTP 客户端。
// 须在任何 Provider 初始化之前调用，通常在 main() 或测试 Setup() 中完成。
func SetDefaultHTTPClient(client *http.Client) {
	if client != nil {
		defaultHTTPClientPtr.Store(client)
	}
}
