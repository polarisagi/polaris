package policy

import (
	"path/filepath"
	"strings"
)

// 磁盘擦除与安全姿态类命令（ADR-0122 决策三）：关闭防火墙/系统完整性保护、提权入组、
// 加载内核模块、开监听口——任何一项都等于永久削弱本机防线，一律灾难性（硬限制询问）。

func postureV(*cc) verdict { return catastropheV("security_posture") }

func (t *cmdTable) addSecurityPosture() {
	t.set(postureV, "insmod", "modprobe", "kextload", "chroot")
	t.addFirewallPosture()
	t.addAccountPosture()
}

func (t *cmdTable) addFirewallPosture() {
	posture := postureV
	t.set(func(c *cc) verdict {
		if hasWord(positionals(c.args), "disable") {
			return posture(c)
		}
		return unknownV("firewall_change")
	}, "ufw")
	t.set(func(c *cc) verdict {
		if hasShort(c.args, 'F') || hasLong(c.args, "--flush") {
			return posture(c)
		}
		return unknownV("firewall_change")
	}, "iptables", "ip6tables")
	t.set(func(c *cc) verdict {
		pos := positionals(c.args)
		if hasWord(pos, "flush") && hasWord(pos, "ruleset") {
			return posture(c)
		}
		return unknownV("firewall_change")
	}, "nft")
	t.set(func(c *cc) verdict {
		if hasShort(c.args, 'd') {
			return posture(c)
		}
		return unknownV("firewall_change")
	}, "pfctl")
	t.set(func(c *cc) verdict {
		if hasWord(c.words, "advfirewall") || hasWord(c.words, "firewall") {
			if hasWord(c.words, "off") || hasWord(c.words, "disable") {
				return posture(c)
			}
		}
		return unknownV("firewall_change")
	}, "netsh")
	t.set(func(c *cc) verdict {
		if hasWord(c.args, "disable") {
			return posture(c)
		}
		return unknownV("system_integrity")
	}, "csrutil")
	t.set(func(c *cc) verdict {
		if hasLong(c.args, "--master-disable", "--global-disable") {
			return posture(c)
		}
		return unknownV("system_integrity")
	}, "spctl")
}

func (t *cmdTable) addAccountPosture() {
	posture := postureV
	t.set(func(c *cc) verdict {
		if hasWord(positionals(c.args), "root") {
			return posture(c)
		}
		return unknownV("passwd_change")
	}, "passwd")
	t.set(func(c *cc) verdict {
		if grantsPrivilegedGroup(c.args) {
			return posture(c)
		}
		return unknownV("user_management")
	}, "useradd", "usermod", "adduser", "gpasswd", "dseditgroup", "addgroup")
	t.set(func(c *cc) verdict {
		write := hasWord(c.args, "-append") || hasWord(c.args, "-merge") || hasWord(c.args, "-create") ||
			hasWord(c.args, "-change") || hasWord(c.args, "-delete")
		if write && grantsPrivilegedGroup(c.args) {
			return posture(c)
		}
		return unknownV("user_management")
	}, "dscl")
}

func hasWord(words []string, want string) bool {
	for _, w := range words {
		if strings.EqualFold(w, want) {
			return true
		}
	}
	return false
}

// grantsPrivilegedGroup 参数里出现 sudo/wheel/admin/root 组名（含 -G sudo,docker、/Groups/admin 形态）。
func grantsPrivilegedGroup(args []string) bool {
	for _, a := range args {
		a = strings.ToLower(a)
		if strings.HasPrefix(a, "/groups/") {
			a = filepath.Base(a)
		}
		for _, tok := range strings.Split(a, ",") {
			switch tok {
			case "sudo", "wheel", "admin", "root":
				return true
			}
		}
	}
	return false
}

// diskutilHandler 只读子命令放行；擦除/重新分区/抹零灾难性；其余改动类保持危险。
func diskutilHandler(c *cc) verdict {
	sub, rest := c.subcommand()
	l := strings.ToLower(sub)
	switch {
	case l == "list" || l == "info" || l == "activity" || l == "listfilesystems":
		return safeV()
	case strings.HasPrefix(l, "erase"):
		return catastropheV("disk_erase")
	}
	switch l {
	case "zerodisk", "secureerase", "partitiondisk", "repartitiondisk", "reformat", "mergepartitions", "splitpartition":
		return catastropheV("disk_erase")
	case "apfs", "corestorage", "appleraid":
		n, _ := firstNonFlag(rest)
		n = strings.ToLower(n)
		if strings.HasPrefix(n, "delete") || strings.HasPrefix(n, "erase") {
			return catastropheV("disk_erase")
		}
		if n == "list" {
			return safeV()
		}
	}
	return dangerV("disk_partition")
}

// isBlockDevice 写入这些设备等同擦盘。/dev/null、/dev/zero、tty 等伪设备不在其列。
func isBlockDevice(p string) bool {
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	name := strings.TrimPrefix(p, "/dev/")
	for _, pre := range []string{"sd", "hd", "vd", "xvd", "nvme", "mmcblk", "disk", "rdisk", "loop", "dm-", "md", "sr", "mapper/"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}
