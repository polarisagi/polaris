// 已废弃：hook 的 Linux namespace 隔离存根。
// 原 hookSysProcAttr() 及其对 sandbox.ContainerSandboxSysProcAttr() 的调用已删除。
// hook/runner.go 的 runCommand 不再注入 SysProcAttr；
// 安全边界由 PolicyGate + 最小 env 变量（PATH 白名单）提供。
// 保留此文件仅为 git 历史可追溯。

//go:build linux

package hook
