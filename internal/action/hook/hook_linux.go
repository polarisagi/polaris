//go:build linux

package hook

import (
	"syscall"

	"github.com/polarisagi/polaris/internal/sandbox"
)

// hookSysProcAttr 返回 Linux 专属的 Hook 子进程安全属性（PID + 挂载 namespace 隔离）。
// 与 ContainerSandbox.RunScript 保持一致的隔离策略。
// 加入 CLONE_NEWUSER 及映射，允许普通用户（如 CI 或 Tier-0 运行环境）创建 namespace。
func hookSysProcAttr() *syscall.SysProcAttr {
	return sandbox.ContainerSandboxSysProcAttr()
}
