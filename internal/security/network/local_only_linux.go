//go:build linux

package network

import (
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func (s *OSNetworkSandbox) enableOS() error {
	// LANDLOCK_ACCESS_NET_BIND_TCP=1, LANDLOCK_ACCESS_NET_CONNECT_TCP=2（内核≥5.19，ABI 2）
	const (
		landlockAccessNetBindTCP    uint64 = 1 << 0
		landlockAccessNetConnectTCP uint64 = 1 << 1
	)
	attr := unix.LandlockRulesetAttr{
		Access_net: landlockAccessNetBindTCP | landlockAccessNetConnectTCP,
	}
	// 创建 ruleset，声明要限制的网络操作类型
	r0, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return apperr.New(apperr.CodeInternal,
			"local_only: Landlock ABI 2 (kernel ≥ 5.19) not available; L1 OS sandbox disabled, L2/L3 active")
	}
	fd := int(r0)
	// 不添加任何 allow 规则 → 所有受限操作均被拒绝
	// 调用 RESTRICT_SELF 将限制应用到当前进程
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0)
	_ = unix.Close(fd)
	if errno != 0 {
		return apperr.New(apperr.CodeInternal,
			"local_only: landlock_restrict_self failed; L1 OS sandbox disabled, L2/L3 active")
	}
	return nil
}
