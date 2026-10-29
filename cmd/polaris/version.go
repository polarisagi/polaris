package main

// 版本信息通过 ldflags 在构建期注入：
//
//	go build -ldflags="-X main.Version=v1.2.3 -X main.CommitHash=abc1234 -X main.BuildDate=2025-01-01T00:00:00Z"
//
// GoReleaser 在 .goreleaser.yaml 中自动注入，开发构建显示 "dev"。
var (
	Version    = "dev"
	CommitHash = "unknown"
	BuildDate  = "unknown"
)
