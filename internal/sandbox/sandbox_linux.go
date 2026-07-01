// 已废弃：Linux namespace 隔离路径（CLONE_NEWUSER/PID/NS）统一替换为 Rust bwrap 沙箱。
// 原 ContainerSandboxSysProcAttr / containerSandboxSysProcAttr 已删除。
// 所有命令执行统一走 CmdRunner → WrapBashCmdRunner → bwrap。
// 保留此文件仅为 git 历史可追溯。

//go:build linux

package sandbox
