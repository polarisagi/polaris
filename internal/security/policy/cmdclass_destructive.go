package policy

import (
	"path/filepath"
	"runtime"
	"strings"
)

func (t *cmdTable) addWorkspaceWrite() {
	t.set(mkdirTouchHandler, "mkdir", "touch")
	t.set(cpHandler, "cp")
	t.set(mvHandler, "mv")
	t.set(teeHandler, "tee")
}

func mkdirTouchHandler(c *cc) verdict {
	pos := positionals(c.args)
	if len(pos) == 0 {
		return safeV()
	}
	return c.writePaths(pos)
}

// cp 只校验目的地：源可以在任何位置（读取），目的地必须在可信根内。
func cpHandler(c *cc) verdict {
	if hasShort(c.args, 't') || hasLong(c.args, "--target-directory") {
		return unknownV("cp_target_directory")
	}
	pos := positionals(c.args)
	if len(pos) < 2 {
		return unknownV("cp_usage")
	}
	return c.writePaths(pos[len(pos)-1:])
}

// mv 会移除源，源和目的地都必须在可信根内。
func mvHandler(c *cc) verdict {
	if hasShort(c.args, 't') || hasLong(c.args, "--target-directory") {
		return unknownV("mv_target_directory")
	}
	pos := positionals(c.args)
	if len(pos) < 2 {
		return unknownV("mv_usage")
	}
	return c.writePaths(pos)
}

func teeHandler(c *cc) verdict {
	pos := positionals(c.args)
	if len(pos) == 0 {
		return safeV()
	}
	return c.writePaths(pos)
}

func (t *cmdTable) addDestructive() {
	t.set(rmHandler, "rm", "rmdir", "unlink", "shred", "trash")
	t.set(chmodHandler, "chmod", "chown", "chgrp")
	t.set(ddHandler, "dd")
	t.set(killHandler, "kill")
	t.set(func(*cc) verdict { return catastropheV("privilege_escalation") }, "sudo", "su", "doas", "pkexec")
	t.set(func(*cc) verdict { return catastropheV("format_filesystem") }, "mkfs", "mkfs.ext2", "mkfs.ext3", "mkfs.ext4",
		"mkfs.xfs", "mkfs.vfat", "mkfs.fat", "mkfs.btrfs", "mkfs.ntfs", "mkfs.exfat", "mkfs.f2fs", "mkdosfs", "mke2fs",
		"mkswap", "newfs")
	t.set(func(*cc) verdict { return catastropheV("disk_erase") }, "wipefs")
	t.set(diskutilHandler, "diskutil")
	t.set(func(*cc) verdict { return dangerV("system_power") }, "shutdown", "reboot", "halt", "poweroff", "init", "telinit")
	t.set(func(*cc) verdict { return dangerV("persistence_or_system_service") }, "crontab", "at", "batch", "launchctl", "systemctl", "service")
	t.set(func(*cc) verdict { return dangerV("disk_partition") }, "fdisk", "parted", "gdisk", "sfdisk")
	t.set(func(*cc) verdict { return dangerV("shell_eval") }, "eval", "source", ".")
	t.set(func(c *cc) verdict { return unknownV("process_kill") }, "killall", "pkill")
	t.addSecurityPosture()
	t.set(func(*cc) verdict { return unknownV("exec_replace") }, "exec")
}

// systemTopDirs rm -rf 这些目录等同毁坏系统，归灾难性。
func systemTopDirs() map[string]bool {
	m := map[string]bool{}
	for _, d := range []string{"/", "/usr", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/dev", "/proc", "/sys",
		"/var", "/opt", "/System", "/Library", "/Applications", "/Users", "/home", "/private", "/Volumes", "/root"} {
		m[d] = true
	}
	return m
}

func rmHandler(c *cc) verdict {
	pos := positionals(c.args)
	recursive := hasShort(c.args, 'r') || hasShort(c.args, 'R') || hasLong(c.args, "--recursive")
	force := hasShort(c.args, 'f') || hasLong(c.args, "--force")
	if len(pos) == 0 {
		return unknownV("rm_usage")
	}
	var worst verdict
	for i, p := range pos {
		v := c.rmTarget(p, recursive, force)
		if i == 0 || v.cls > worst.cls {
			worst = v
		}
	}
	return worst
}

func (c *cc) rmTarget(p string, recursive, force bool) verdict {
	if hasDyn(p) {
		if recursive {
			if r := c.dynRootReason(p); r != "" {
				return catastropheV(r)
			}
		}
		if recursive || force {
			return dangerV("rm_dynamic_target")
		}
		return unknownV("rm_dynamic_target")
	}
	if r := c.catastrophicTarget(p); r != "" {
		return catastropheV(r)
	}
	if c.w.pathState(p, false) == pathInside {
		return unknownV("rm_in_workspace")
	}
	return dangerV("rm_outside_workspace")
}

// dynRootReason 递归删除目标以运行期展开开头（$X/…、$(…)…），或展开之前的字面前缀本身已是灾难性目标时，
// 空变量会让命令退化成 rm -rf / 或 rm -rf <灾难性目录>（ADR-0122 决策三）。
func (c *cc) dynRootReason(p string) string {
	prefix := p[:strings.Index(p, DynToken)]
	if prefix == "" {
		return "rm_dynamic_root"
	}
	return c.catastrophicTarget(prefix)
}

// homeUserDirs 家目录一级用户数据目录：被整体删除即不可恢复地丢失用户数据。
func homeUserDirs() []string {
	return []string{"Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Library", ".config", ".local"}
}

// samePath 路径逐字相等；darwin/windows 的默认文件系统大小写不敏感，故折叠大小写。
func samePath(a, b string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// catastrophicTarget 判定删除类目标是否灾难性并返回原因码，空串表示不是。含运行期展开的词不应传入。
func (c *cc) catastrophicTarget(p string) string {
	trimmed := strings.TrimRight(p, "/")
	if trimmed == "" || trimmed == "~" || trimmed == "/*" || trimmed == "~/*" {
		return "rm_system_or_home"
	}
	base := strings.TrimSuffix(trimmed, "/*")
	if base == "*" {
		base = "." // 裸 * 即 cwd 下全部内容
	}
	if samePath(filepath.Base(base), ".git") {
		return "rm_git_dir"
	}
	abs, ok := absPath(base, c.w.cwd, c.w.env.Home)
	if !ok {
		return ""
	}
	abs = filepath.Clean(abs)
	real := resolveReal(abs)
	if c.isHomeOrSystem(abs, real) {
		return "rm_system_or_home"
	}
	if c.isHomeUserDir(abs, real) {
		return "rm_home_userdir"
	}
	for _, r := range c.w.roots {
		if samePath(real, r) || samePath(abs, r) {
			return "rm_workspace_root"
		}
	}
	return ""
}

func (c *cc) isHomeOrSystem(abs, real string) bool {
	if home := strings.TrimRight(c.w.env.Home, "/"); home != "" {
		for _, h := range []string{home, resolveReal(home)} {
			if samePath(abs, h) || samePath(real, h) {
				return true
			}
		}
	}
	for d := range systemTopDirs() {
		if samePath(abs, d) || samePath(real, d) {
			return true
		}
	}
	return false
}

func (c *cc) isHomeUserDir(abs, real string) bool {
	home := strings.TrimRight(c.w.env.Home, "/")
	if home == "" {
		return false
	}
	for _, h := range []string{home, resolveReal(home)} {
		for _, d := range homeUserDirs() {
			if samePath(abs, filepath.Join(h, d)) || samePath(real, filepath.Join(h, d)) {
				return true
			}
		}
	}
	return false
}

func chmodHandler(c *cc) verdict {
	pos := positionals(c.args)
	if len(pos) < 2 {
		return unknownV("chmod_usage")
	}
	if hasShort(c.args, 'R') || hasLong(c.args, "--recursive") {
		for _, p := range pos[1:] {
			if !hasDyn(p) && c.catastrophicTarget(p) == "rm_system_or_home" {
				return catastropheV("chmod_recursive_system")
			}
		}
	}
	switch c.pathsState(pos[1:]) {
	case pathOutside:
		return dangerV("chmod_outside_workspace")
	case pathInside:
		return unknownV("chmod_in_workspace")
	}
	return unknownV("chmod_unresolved")
}

func ddHandler(c *cc) verdict {
	for _, a := range c.args {
		if !strings.HasPrefix(a, "of=") {
			continue
		}
		target := strings.TrimPrefix(a, "of=")
		if strings.HasPrefix(target, "/dev/") && target != "/dev/null" {
			return catastropheV("dd_write_device")
		}
		if c.w.pathState(target, hasDyn(target)) == pathOutside {
			return dangerV("dd_outside_workspace")
		}
	}
	return unknownV("dd_usage")
}

func killHandler(c *cc) verdict {
	for _, a := range c.args {
		if a == "1" {
			return dangerV("kill_init_or_all")
		}
	}
	if n := len(c.args); n >= 2 && c.args[n-1] == "-1" {
		return dangerV("kill_init_or_all") // kill -9 -1：向所有进程发信号
	}
	return unknownV("process_kill")
}
