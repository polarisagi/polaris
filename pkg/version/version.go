// Package version 提供构建期注入的版本信息，供 CLI、HTTP 健康检查端点及外部工具引用。
//
// 注入方式（Makefile / GoReleaser）：
//
//	go build -ldflags="-X github.com/polarisagi/polaris/pkg/version.Version=v1.2.3 \
//	  -X github.com/polarisagi/polaris/pkg/version.CommitHash=abc1234 \
//	  -X github.com/polarisagi/polaris/pkg/version.BuildDate=2025-01-01T00:00:00Z"
//
// cmd/polaris/version.go 中的 main 包变量应迁移到此包，cmd 层通过 version.Get() 读取。
package version

import "runtime"

// 以下变量由构建系统通过 -ldflags 在编译期注入。
// 未注入时保持 "dev" / "unknown" 默认值，方便本地开发调试。
var (
	// Version 是 semver 格式的发布版本号（如 "v1.2.3"）。
	Version = "dev"

	// CommitHash 是当前 Git commit 的短哈希（如 "abc1234"）。
	CommitHash = "unknown"

	// BuildDate 是构建时间戳（ISO 8601，如 "2025-01-01T00:00:00Z"）。
	BuildDate = "unknown"
)

// Info 是版本信息的结构化表示，供 JSON 序列化（健康检查 /healthz 响应体等）。
type Info struct {
	Version    string `json:"version"`
	CommitHash string `json:"commit_hash"`
	BuildDate  string `json:"build_date"`
	GoVersion  string `json:"go_version"`
}

// Get 返回当前构建的完整版本信息快照。
func Get() Info {
	return Info{
		Version:    Version,
		CommitHash: CommitHash,
		BuildDate:  BuildDate,
		GoVersion:  runtime.Version(),
	}
}

// String 返回紧凑的单行版本字符串，适合 CLI 展示。
// 格式："{version} ({commit_hash}, built {build_date})"
func String() string {
	return Version + " (" + CommitHash + ", built " + BuildDate + ")"
}
