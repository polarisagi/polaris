// Package classifier — rules.go
//
// 默认静态规则集。规则按 DENY → HITL → WARN 顺序定义，Classify() 会取最高命中等级。
//
// 规则维护原则：
//   - 优先误报（HITL）而非漏报（SAFE）——漏报无法弥补，误报由 HITL 兜底
//   - 正则尽量精确，避免匹配过宽导致正常命令被拦截
//   - 新增规则需同步在 classifier_test.go 中添加至少一条正例和一条反例
//   - (?i) 前缀表示大小写不敏感匹配

package classifier

// defaultRules 返回内置默认规则集（按 DENY → HITL → WARN 顺序）。
func defaultRules() []Rule {
	return append(append(denyRules(), hitlRules()...), warnRules()...)
}

// ─── DENY：不可逆高危操作，直接拒绝 ─────────────────────────────────────────

func denyRules() []Rule {
	return []Rule{
		// Fork bomb
		{RiskDeny, "fork bomb detected", `:\(\)\s*\{.*:\|.*&.*\}`},

		// 写 /dev/zero 或 /dev/null 到磁盘设备（磁盘擦除）
		{RiskDeny, "disk wipe via dd", `(?i)\bdd\b.*\bif=/dev/(zero|random|urandom)\b.*\bof=/dev/`},
		{RiskDeny, "disk wipe via dd (reversed)", `(?i)\bdd\b.*\bof=/dev/[a-z]+\d*\b.*\bif=/dev/(zero|random)`},

		// 递归覆盖根目录
		{RiskDeny, "rm -rf / or system root", `(?i)\brm\b.*-[a-z]*r[a-z]*f[a-z]*\s+/\s*$`},
		{RiskDeny, "rm -rf / with trailing", `(?i)\brm\b.*-[a-z]*r[a-z]*f[a-z]*\s+/[^a-zA-Z0-9_\-.]`},

		// 格式化文件系统
		{RiskDeny, "filesystem format", `(?i)\b(mkfs|mke2fs|mkswap|wipefs)\b`},

		// 禁用防火墙
		{RiskDeny, "disable firewall", `(?i)\b(ufw\s+disable|iptables\s+-F|nft\s+flush\s+ruleset)\b`},

		// 操作系统用户修改（提权）
		{RiskDeny, "user/group modification", `(?i)\b(adduser|useradd|usermod|groupadd|groupmod)\b.*(-G\s*(sudo|wheel|root)|--groups\s*(sudo|wheel|root))`},
		{RiskDeny, "passwd modification", `(?i)\bpasswd\b\s+root`},

		// 网络监听后门
		{RiskDeny, "network backdoor listener", `(?i)\b(nc|ncat|netcat)\b.*-[a-z]*l[a-z]*\b`},

		// 写 /etc/passwd 或 /etc/shadow
		{RiskDeny, "write to /etc/passwd or /etc/shadow", `(?i)(>|>>|tee)\s*/etc/(passwd|shadow|sudoers)`},

		// 管道远程代码执行（curl/wget 管道到 sh/bash/python）
		{RiskDeny, "remote code execution via pipe", `(?i)(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba)?sh\b`},
		{RiskDeny, "remote code execution via pipe python", `(?i)(curl|wget)\b[^|]*\|\s*(sudo\s+)?python[23]?\b`},

		// 内核模块加载
		{RiskDeny, "kernel module load", `(?i)\b(insmod|modprobe)\b`},

		// chroot 逃逸尝试
		{RiskDeny, "chroot escape attempt", `(?i)\bchroot\b`},
	}
}

// ─── HITL：高风险操作，暂停等待人工审批 ────────────────────────────────────

func hitlRules() []Rule {
	return []Rule{
		// rm -rf（非根目录）
		{RiskHITL, "recursive force remove", `(?i)\brm\b\s+.*-[a-z]*r[a-z]*f[a-z]*`},
		{RiskHITL, "recursive force remove (flags first)", `(?i)\brm\b\s+-[a-z]*r[a-z]*f[a-z]*`},

		// sudo（权限提升）
		{RiskHITL, "privilege escalation via sudo", `(?i)\bsudo\b`},

		// 包管理器安装（影响系统/全局环境）
		{RiskHITL, "pip global install", `(?i)\bpip[23]?\b\s+install\b`},
		{RiskHITL, "npm global install", `(?i)\bnpm\b\s+install\s+-g\b`},
		{RiskHITL, "apt/apt-get/yum/dnf install", `(?i)\b(apt-get|apt|yum|dnf|brew)\b\s+install\b`},
		{RiskHITL, "cargo install", `(?i)\bcargo\b\s+install\b`},

		// 网络请求（数据外泄风险）
		{RiskHITL, "outbound HTTP request via curl", `(?i)\bcurl\b`},
		{RiskHITL, "outbound HTTP request via wget", `(?i)\bwget\b`},
		{RiskHITL, "outbound HTTP request via httpie", `(?i)\bhttp\b\s+(GET|POST|PUT|DELETE|PATCH)\b`},

		// 远程操作
		{RiskHITL, "remote shell via ssh", `(?i)\bssh\b`},
		{RiskHITL, "remote copy via scp", `(?i)\bscp\b`},
		{RiskHITL, "remote sync via rsync", `(?i)\brsync\b`},
		{RiskHITL, "ftp transfer", `(?i)\b(ftp|sftp)\b`},

		// Git 远程写操作
		{RiskHITL, "git push to remote", `(?i)\bgit\b\s+push\b`},
		{RiskHITL, "git force push", `(?i)\bgit\b\s+push\b.*--force`},

		// 定时任务 / 系统服务
		{RiskHITL, "crontab modification", `(?i)\bcrontab\b\s+-[eil]`},
		{RiskHITL, "systemctl start/stop/enable/disable", `(?i)\bsystemctl\b\s+(start|stop|restart|enable|disable|mask)\b`},
		{RiskHITL, "service control", `(?i)\bservice\b\s+\w+\s+(start|stop|restart)\b`},

		// 容器 / 集群操作
		{RiskHITL, "docker run/exec", `(?i)\bdocker\b\s+(run|exec|rm|rmi|stop|kill)\b`},
		{RiskHITL, "kubectl apply/delete", `(?i)\bkubectl\b\s+(apply|delete|patch|exec)\b`},

		// 写 /etc/ 目录（非 passwd/shadow，那些已在 DENY）
		{RiskHITL, "write to /etc/ directory", `(?i)(>|>>|tee)\s*/etc/`},

		// 大范围权限修改
		{RiskHITL, "recursive chmod", `(?i)\bchmod\b\s+.*-[a-z]*R[a-z]*\b`},
		{RiskHITL, "recursive chown", `(?i)\bchown\b\s+.*-[a-z]*R[a-z]*\b`},
		{RiskHITL, "chmod 777", `(?i)\bchmod\b\s+(777|a\+rwx)\b`},

		// 进程终止（批量）
		{RiskHITL, "pkill / killall", `(?i)\b(pkill|killall)\b`},
		{RiskHITL, "kill -9 all", `(?i)\bkill\b\s+-9\s+-1\b`},

		// 数据库操作
		{RiskHITL, "drop database/table", `(?i)\b(DROP\s+(DATABASE|TABLE|SCHEMA)|TRUNCATE\s+TABLE)\b`},

		// 写入/删除 .ssh 目录
		{RiskHITL, "modify SSH keys", `(?i)(>|>>|rm\b.*)\s*~?/?(home/\w+/)?\.ssh/`},

		// Python/Node 脚本下载执行
		{RiskHITL, "python -c with network", `(?i)\bpython[23]?\b\s+-c\b.*__(import|urllib|socket)`},
	}
}

// ─── WARN：有副作用但可逆的操作，执行 + 强化审计 ───────────────────────────

func warnRules() []Rule {
	return []Rule{
		// 文件删除（非递归）
		{RiskWarn, "file removal", `(?i)\brm\b\s+(?!.*-[a-z]*r)`},

		// 文件移动（潜在数据丢失）
		{RiskWarn, "file move", `(?i)\bmv\b`},

		// 大范围复制
		{RiskWarn, "recursive copy", `(?i)\bcp\b\s+.*-[a-z]*r[a-z]*\b`},

		// Git 本地写操作（commit/merge/rebase/reset）
		{RiskWarn, "git commit", `(?i)\bgit\b\s+commit\b`},
		{RiskWarn, "git merge", `(?i)\bgit\b\s+merge\b`},
		{RiskWarn, "git rebase", `(?i)\bgit\b\s+rebase\b`},
		{RiskWarn, "git reset --hard", `(?i)\bgit\b\s+reset\b.*--hard\b`},
		{RiskWarn, "git clean -fd", `(?i)\bgit\b\s+clean\b.*-[a-z]*f[a-z]*d[a-z]*\b`},

		// 进程终止（单个）
		{RiskWarn, "kill process", `(?i)\bkill\b\s+(-[0-9]+\s+)?\d+`},

		// find -delete
		{RiskWarn, "find with delete action", `(?i)\bfind\b.*-delete\b`},

		// 文件内容覆盖（重定向写）
		{RiskWarn, "file overwrite via redirect", `(?i)[^>]>[^>]\s*\S`},

		// 环境变量写出（export 到文件）
		{RiskWarn, "export sensitive env to file", `(?i)\benv\b.*>|printenv\b.*>`},

		// 写 ~/.bashrc / ~/.zshrc / ~/.profile（可持久化 payload）
		{RiskWarn, "modify shell rc files", `(?i)(>|>>)\s*~?/?(home/\w+/)?\.?(bash_?rc|zshrc|profile|bash_profile|zprofile)`},

		// tar 解压到系统路径
		{RiskWarn, "tar extract to system path", `(?i)\btar\b.*-[a-z]*x[a-z]*.*-C\s+/(usr|bin|sbin|lib|etc)\b`},
	}
}
