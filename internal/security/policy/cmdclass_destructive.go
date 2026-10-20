package policy

import (
	"path/filepath"
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
	t.set(func(*cc) verdict { return catastropheV("format_filesystem") }, "mkfs", "mkfs.ext4", "mkfs.xfs",
		"mkfs.vfat", "mkfs.fat", "mkfs.btrfs", "mke2fs", "newfs")
	t.set(func(*cc) verdict { return dangerV("system_power") }, "shutdown", "reboot", "halt", "poweroff", "init", "telinit")
	t.set(func(*cc) verdict { return dangerV("persistence_or_system_service") }, "crontab", "at", "batch", "launchctl", "systemctl", "service")
	t.set(func(*cc) verdict { return dangerV("disk_partition") }, "fdisk", "parted", "diskutil", "gdisk", "sfdisk")
	t.set(func(*cc) verdict { return dangerV("shell_eval") }, "eval", "source", ".")
	t.set(func(c *cc) verdict { return unknownV("process_kill") }, "killall", "pkill")
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
	recursiveForce := hasShort(c.args, 'r') || hasShort(c.args, 'R') || hasShort(c.args, 'f') ||
		hasLong(c.args, "--recursive", "--force")
	if len(pos) == 0 {
		return unknownV("rm_usage")
	}
	worst := unknownV("rm_in_workspace")
	for _, p := range pos {
		v := c.rmTarget(p, recursiveForce)
		if v.cls > worst.cls {
			worst = v
		}
	}
	return worst
}

func (c *cc) rmTarget(p string, recursiveForce bool) verdict {
	if hasDyn(p) {
		if recursiveForce {
			return dangerV("rm_dynamic_target")
		}
		return unknownV("rm_dynamic_target")
	}
	if c.isCatastrophicTarget(p) {
		return catastropheV("rm_system_or_home")
	}
	if c.w.pathState(p, false) == pathInside {
		return unknownV("rm_in_workspace")
	}
	return dangerV("rm_outside_workspace")
}

// isCatastrophicTarget 判定删除目标是否为根、家目录、系统顶层目录或它们的通配。
func (c *cc) isCatastrophicTarget(p string) bool {
	trimmed := strings.TrimRight(p, "/")
	if trimmed == "" || trimmed == "~" || trimmed == "/*" || trimmed == "~/*" || trimmed == "*" && c.w.cwd == "/" {
		return true
	}
	home := strings.TrimRight(c.w.env.Home, "/")
	abs, ok := absPath(strings.TrimSuffix(trimmed, "/*"), c.w.cwd, c.w.env.Home)
	if !ok {
		return false
	}
	abs = filepath.Clean(abs)
	if home != "" && abs == home {
		return true
	}
	return systemTopDirs()[abs]
}

func chmodHandler(c *cc) verdict {
	pos := positionals(c.args)
	if len(pos) < 2 {
		return unknownV("chmod_usage")
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
