package inference

import "net/http"

// defaultHTTPClient 是包级共享的安全 HTTP 客户端。
// 启动时由外层（cmd/polaris/main.go 或 StorageFabric）通过 SetDefaultHTTPClient
// 注入绑定了 SafeDialer 的客户端。
// 若未注入则退化到 http.DefaultClient（仅限单元测试场景）。
//
// 架构约束 inv_M11_05: 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护。
// 调用方（cmd/polaris）启动时必须调用 SetDefaultHTTPClient(substrate.NewSafeHTTPClient(nil))。
var defaultHTTPClient *http.Client = http.DefaultClient

// SetDefaultHTTPClient 注入全局安全 HTTP 客户端。
// 须在任何 Provider 初始化之前调用，通常在 main() 或测试 Setup() 中完成。
func SetDefaultHTTPClient(client *http.Client) {
	if client != nil {
		defaultHTTPClient = client
	}
}
