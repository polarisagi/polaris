// 已废弃：非 Linux 平台的 namespace 隔离存根。
// 原 ContainerSandboxSysProcAttr / containerSandboxSysProcAttr 已删除。
// macOS 现在通过 Rust Seatbelt（bwrap/sandbox-exec）提供进程隔离。
// 保留此文件仅为 git 历史可追溯。

//go:build !linux

package sandbox
