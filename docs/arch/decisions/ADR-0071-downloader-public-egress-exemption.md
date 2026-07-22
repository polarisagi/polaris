# ADR 0071: Downloader Public Egress Exemption

## 状态
已接受

## 上下文
在代码审查过程中（见 `UPGRADE-PROMPT.md` P0-3），发现 `internal/downloader/proxy.go` 中的 `canReachGitHub` 等函数使用了裸 `http.DefaultTransport`，绕过了 M11 `SafeDialer` 的五阶段网络隔离（违反 XR-06 红线）。

然而，该处绕过有其合理动机：
1. `SafeDialer` 有独立的 `DialContext`，无法走系统级的 TUN 隧道。
2. `SafeDialer` 默认不读取环境变量 `HTTPS_PROXY`，但在中国大陆等网络受限地区，必须依赖系统代理或 TUN 才能触达 GitHub 等资源库进行必要的探测。
3. `canReachGitHub` 的目标 URL (`https://github.com`) 是静态写死的公共外部域名，没有用户可控的 URL 输入，因此不存在通常意义上的 SSRF 风险。

若强行要求该函数改用 `SafeDialer`，在不大幅重构 `SafeDialer` 核心组件增加公共网关代理特性的情况下，会导致 Tier-0 本地部署场景下无法正确探测 GitHub 连通性，造成功能退化。

## 决策
我们决定**免除** `internal/downloader/proxy.go` 针对静态公共域名探测使用 `SafeDialer` 的强制要求。
为实现这一点，我们采取以下措施：
1. 记录本 ADR 以在架构和安全层面正式豁免该逻辑。
2. 在 `scripts/xr06-allowlist.txt` 中登记 `internal/downloader/proxy.go`，使 CI lint 扫描网络红线 (XR-06) 时显式放行该文件内的特定用法。
3. 严格限制该豁免仅适用于写死的公共外部域名，任何包含用户输入或动态拼接的 URL 仍必须受 `SafeDialer` 约束。

## 后果
- **正面**：避免了 `SafeDialer` 核心组件因引入非关键特性的复杂修改而可能产生的潜在安全旁路。保持了本地部署代理功能的完整性。
- **负面**：增加了特例，需要在未来的审计和代码变动中持续确保 `proxy.go` 中没有新的、用户可控的请求利用此豁免。

## 参考
- `UPGRADE-PROMPT.md` P0-3
- M11 网络安全基线 (XR-06)
