# ADR-0071: Downloader 公网出站豁免（XR-06）

- **状态**: Accepted（已执行）| **模块**: `internal/downloader/proxy.go`

## 决策

`canReachGitHub` 等函数豁免 M11 SafeDialer（XR-06），允许用裸 `http.DefaultTransport`：SafeDialer 无法走系统 TUN 隧道、不读 `HTTPS_PROXY`，中国大陆等网络受限地区必须依赖系统代理才能触达 GitHub；且目标 URL（`https://github.com`）静态写死非用户可控，不构成 SSRF。

豁免边界由 CI 强制而非仅文档：`internal/lint.Test_inv_XR06_DownloaderNoRawTransport` 扫描 `internal/downloader/` 裸 Transport 引用，`xr06_raw_transport_exempt.json` 登记 `proxy.go` 为唯一豁免项，未登记文件新增裸 Transport 直接编译期失败。豁免仅适用静态写死的公共外部域名，任何用户输入/动态拼接 URL 仍须受 SafeDialer 约束。

## 引用代码

`internal/downloader/proxy.go`、`internal/lint/testdata/xr06_raw_transport_exempt.json`
