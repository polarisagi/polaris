// Package tool — native_sandbox.go
//
// 平台原生进程沙箱封装。
//
// 架构：优先调用 Rust FFI（native_sandbox_exec），Rust dylib 不可用时降级为 Go 本地实现。
//   Rust 实现（优先）: macOS Seatbelt / Linux bwrap / Windows WSL2 + 自动 PATH 探测
//   Go 降级（备用）  : 同平台逻辑，用于 dylib 未构建的开发环境
//
// 参照：Claude Code sandboxing（anthropic.com/engineering/claude-code-sandboxing）
//       Codex CLI sandbox modes（developers.openai.com/codex/concepts/sandboxing）

package sandbox
