//go:build windows

package ollamamgr

import (
	"os/exec"
	"syscall"
)

// BELOW_NORMAL_PRIORITY_CLASS Windows 进程低优先级标志
const belowNormalPriorityClass = 0x00004000

func prepareCmdAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: belowNormalPriorityClass,
	}
}

func setProcessLowPriority(cmd *exec.Cmd) {
	// Windows 在进程创建时已通过 CreationFlags 设置
}
