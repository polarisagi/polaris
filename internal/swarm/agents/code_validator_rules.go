package agents

import "regexp"

// codeValidatorRules 包含所有语言的安全扫描配置。
type codeValidatorRules struct {
	pythonDangerousPatterns     []codeRule
	bashDangerousPatterns       []codeRule
	wasiAllowedImports          map[string]bool
	wasiCapabilityGatedImports  map[string]string
	typescriptDangerousPatterns []codeRule
	dangerousGoPackages         map[string]string
	pythonDangerousImports      map[string]string
	bashDangerousCommands       map[string]string
	tsDangerousImports          map[string]string
}

func newCodeValidatorRules() *codeValidatorRules {
	return &codeValidatorRules{
		pythonDangerousPatterns: []codeRule{
			{
				id:          "PY001",
				description: "动态代码执行：exec() / eval()",
				requiredCap: "dynamic_eval",
				pattern:     regexp.MustCompile(`\b(exec|eval)\s*\(`),
			},
			{
				id:          "PY002",
				description: "动态导入：__import__()",
				requiredCap: "dynamic_import",
				pattern:     regexp.MustCompile(`__import__\s*\(`),
			},
			{
				id:          "PY003",
				description: "Shell 命令执行：os.system() / os.popen()",
				requiredCap: "shell_exec",
				pattern:     regexp.MustCompile(`os\.(system|popen|exec[lv][pe]?)\s*\(`),
			},
			{
				id:          "PY004",
				description: "子进程启动：subprocess 模块",
				requiredCap: "shell_exec",
				pattern:     regexp.MustCompile(`subprocess\.(run|call|Popen|check_output)\s*\(`),
			},
			{
				id:          "PY005",
				description: "网络 socket 原始调用",
				requiredCap: "network_raw",
				pattern:     regexp.MustCompile(`socket\.socket\s*\(`),
			},
			{
				id:          "PY006",
				description: "危险路径写入：写入 /etc、/sys、/proc、/boot",
				requiredCap: "system_write",
				pattern:     regexp.MustCompile(`open\s*\(\s*["'](/etc|/sys|/proc|/boot|/root)`),
			},
			{
				id:          "PY007",
				description: "ctypes 内存直接操作（旁路沙箱）",
				requiredCap: "native_memory",
				pattern:     regexp.MustCompile(`\bctypes\b`),
			},
		},
		bashDangerousPatterns: []codeRule{
			{id: "SH001", description: "危险递归删除", requiredCap: "destructive_fs",
				pattern: regexp.MustCompile(`rm\s+(-[rfR]+\s+)?(/|~|\$HOME|\*|\$\w+)`)},
			{id: "SH002", description: "格式化磁盘", requiredCap: "destructive_fs",
				pattern: regexp.MustCompile(`\b(mkfs|dd\s+.*of=/dev|shred)\b`)},
			{id: "SH003", description: "修改系统文件", requiredCap: "system_write",
				pattern: regexp.MustCompile(`\b(chmod|chown|chattr)\b.*(/etc|/sys|/boot)`)},
			{id: "SH004", description: "curl/wget 管道执行", requiredCap: "network_exec",
				pattern: regexp.MustCompile(`(curl|wget).*\|\s*(bash|sh|python)`)},
			{id: "SH005", description: "后台驻留进程", requiredCap: "background_process",
				pattern: regexp.MustCompile(`nohup\s|&\s*$|disown`)},
		},
		wasiAllowedImports: map[string]bool{
			"wasi_snapshot_preview1:fd_write":            true, // stdout/stderr 输出
			"wasi_snapshot_preview1:fd_read":             true, // stdin 读取
			"wasi_snapshot_preview1:fd_close":            true,
			"wasi_snapshot_preview1:fd_seek":             true,
			"wasi_snapshot_preview1:fd_fdstat_get":       true,
			"wasi_snapshot_preview1:args_get":            true, // 命令行参数
			"wasi_snapshot_preview1:args_sizes_get":      true,
			"wasi_snapshot_preview1:environ_get":         true, // 环境变量读取
			"wasi_snapshot_preview1:environ_sizes_get":   true,
			"wasi_snapshot_preview1:clock_time_get":      true, // 时钟
			"wasi_snapshot_preview1:proc_exit":           true, // 正常退出
			"wasi_snapshot_preview1:random_get":          true, // 随机数
			"wasi_snapshot_preview1:path_open":           true, // VFS 路径（受沙箱 preopens 限制）
			"wasi_snapshot_preview1:path_read_link":      true,
			"wasi_snapshot_preview1:fd_readdir":          true,
			"wasi_snapshot_preview1:fd_prestat_get":      true,
			"wasi_snapshot_preview1:fd_prestat_dir_name": true,
		},
		wasiCapabilityGatedImports: map[string]string{
			"wasi_snapshot_preview1:sock_open":             "network_raw",
			"wasi_snapshot_preview1:sock_connect":          "network_raw",
			"wasi_snapshot_preview1:sock_send":             "network_raw",
			"wasi_snapshot_preview1:sock_recv":             "network_raw",
			"wasi_snapshot_preview1:sock_accept":           "network_listen",
			"wasi_snapshot_preview1:path_remove_directory": "destructive_fs",
			"wasi_snapshot_preview1:path_unlink_file":      "destructive_fs",
			"wasi_snapshot_preview1:path_rename":           "destructive_fs",
			"wasi_snapshot_preview1:path_create_directory": "fs_write",
			"wasi_snapshot_preview1:proc_raise":            "process_signal",
		},
		typescriptDangerousPatterns: []codeRule{
			{
				id:          "TS001",
				description: "动态代码执行：eval() / new Function()",
				requiredCap: "dynamic_eval",
				pattern:     regexp.MustCompile(`\beval\s*\(|new\s+Function\s*\(`),
			},
			{
				id:          "TS002",
				description: "子进程启动：child_process 模块",
				requiredCap: "shell_exec",
				// require/import 两种导入方式均覆盖
				pattern: regexp.MustCompile(`require\s*\(\s*['"]child_process['"]\s*\)|from\s+['"]child_process['"]`),
			},
			{
				id:          "TS003",
				description: "动态变量导入：import(variable)，无法静态分析目标模块",
				requiredCap: "dynamic_import",
				// 匹配 import() 且括号内不是字符串字面量（字符串以引号开头）
				pattern: regexp.MustCompile(`\bimport\s*\(\s*[^"'\x60]`),
			},
			{
				id:          "TS004",
				description: "进程强制退出或发送信号",
				requiredCap: "process_signal",
				pattern:     regexp.MustCompile(`process\.(exit|kill|abort)\s*\(`),
			},
			{
				id:          "TS005",
				description: "文件系统破坏性操作：rm / unlink / rmdir",
				requiredCap: "destructive_fs",
				pattern:     regexp.MustCompile(`fs\.(rm|unlink|rmdir|rmdirSync|unlinkSync|rmSync)\s*\(`),
			},
			{
				id:          "TS006",
				description: "base64 decode chained with eval/Function constructor (proximity ≤200 chars)",
				requiredCap: "dynamic_eval",
				// (?s) 已移除；.{0,200} 限制 decode→eval 间距，避免跨函数/文件误报
				pattern: regexp.MustCompile(`(atob\s*\(|Buffer\.from\s*\([^)]+,\s*['"]base64['"]\s*\)).{0,200}(eval\s*\(|new\s+Function\s*\()`),
			},
			{
				id:          "TS007",
				description: "危险路径写入：写入 /etc、/sys、/proc、/boot",
				requiredCap: "system_write",
				pattern:     regexp.MustCompile(`writeFile[Ss]ync?\s*\(\s*["'](/etc|/sys|/proc|/boot|/root)`),
			},
			{
				id:          "TS008",
				description: "网络原始 Socket（绕过 HTTP 层）",
				requiredCap: "network_raw",
				pattern:     regexp.MustCompile(`require\s*\(\s*['"]net['"]\s*\)|from\s+['"]net['"]`),
			},
		},
		dangerousGoPackages: map[string]string{
			"os/exec":       "shell_exec",
			"syscall":       "shell_exec",
			"unsafe":        "native_memory",
			"net":           "network_raw",
			"net/http":      "network_raw",
			"crypto/tls":    "network_raw",
			"plugin":        "shell_exec",
			"runtime/debug": "native_memory",
		},
		pythonDangerousImports: map[string]string{
			"subprocess": "shell_exec",
			"os":         "shell_exec", // os.system / os.popen
			"pty":        "shell_exec",
			"socket":     "network_raw",
			"urllib":     "network_raw",
			"requests":   "network_raw",
			"ctypes":     "native_memory",
			"cffi":       "native_memory",
		},
		bashDangerousCommands: map[string]string{
			"curl ":  "network_raw",
			"wget ":  "network_raw",
			"nc ":    "network_raw",
			"ncat ":  "network_raw",
			"eval ":  "shell_exec",
			"exec ":  "shell_exec",
			"chmod ": "shell_exec",
			"chown ": "shell_exec",
		},
		tsDangerousImports: map[string]string{
			"child_process":  "shell_exec",
			"fs":             "shell_exec",
			"net":            "network_raw",
			"http":           "network_raw",
			"https":          "network_raw",
			"vm":             "shell_exec",
			"worker_threads": "shell_exec",
		},
	}
}
