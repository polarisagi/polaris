# inv_safe_dialer_01 棘轮基线

判据见 `tools/safe_dialer_lint.go` 头部。格式：每行以 `path:line` 开头，其后为理由。
**只禁增量**——新写的出站入口必须经 SafeDialer。

## 背景（2026-08-17）

本规则此前与 `internal/lint` 的 `Test_inv_M11_05_NoRawNetDial` /
`Test_inv_M1_01_NoRawHTTPCalls` 是同一判据的两份实现，且各自放过对方能抓的形态。
合并取并集后，`http.DefaultTransport` 的**非调用引用**首次进入判据面，一次暴露 4 处
——两套系统此前都没看：内联测试版只禁 `DefaultClient` 不禁 `DefaultTransport`，
而禁 `DefaultTransport` 的 `Test_inv_M13_06` 只扫 `internal/channel`。

## 存量

- internal/downloader/proxy.go:146 `canReachGitHub` 探测 github.com 可达性。**刻意豁免**，
  理由已写在该函数上方注释：目标是公共外部固定域名，不接受任何调用方输入，
  不存在 SSRF 面。重评触发条件：探测目标改为可配置。

以下三条是**同一个形态**：`Transport/Inner == nil` 时回落到 `http.DefaultTransport`。
它们不是"故意裸调"，而是**nil 安全门放行**——CLAUDE.md §不变量 HE-2 的核心禁止项之一。
正常路径都注入了 SafeDialer，但一旦注入方漏传，防护会静默消失且无任何可观测信号
（HE-1 也要求这种降级要能被看见）。判定为**存疑存量，待定夺**，不是已裁定合法：

- internal/downloader/proxy.go:162 `raceFastestMirror` 的 `baseClient == nil` 回落。
  当前镜像清单是内置常量，SSRF 面窄；但回落本身无日志、无指标。
- internal/llm/adapter/anthropic.go:62 `client.Transport == nil` 回落。LLM base URL 可配置，
  这条的 SSRF 面比上一条大。
- internal/llm/rate_tracker.go:195 `RateLimitCapturingTransport.Inner == nil` 回落。
  该 Transport 是包装器，Inner 由装配方注入。

三条的正解方向一致：nil 时返回错误或使用 SafeDialer 承载的 transport，而不是回落到
全局默认；若确需回落，至少要 `slog.Warn` 留痕，使"本次未受保护"这件事可被观测。
定夺后请从本基线移除对应行。
