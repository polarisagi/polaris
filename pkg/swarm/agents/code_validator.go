package agents

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"regexp"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// CapabilitySet 表示沙箱能力集，用于权限判断
type CapabilitySet map[string]bool

type codeRule struct {
	id          string
	description string
	requiredCap string
	pattern     *regexp.Regexp
}

// pythonDangerousPatterns Python 危险调用规则表。
var pythonDangerousPatterns = []codeRule{
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
}

// bashDangerousPatterns Bash 危险调用规则表。
var bashDangerousPatterns = []codeRule{
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
}

func validateByPatterns(code []byte, rules []codeRule, caps CapabilitySet) error {
	for _, rule := range rules {
		if rule.pattern.Match(code) {
			if !caps[rule.requiredCap] {
				return perrors.New(perrors.CodeForbidden, fmt.Sprintf(
					"code validation failed: rule %s (%s) triggered, requires capability '%s'",
					rule.id, rule.description, rule.requiredCap))
			}
		}
	}
	return nil
}

// ValidateCode 扫描并校验生成代码的安全性（正则规则引擎）
func (ga *GovernanceAgent) ValidateCode(language string, code []byte, caps CapabilitySet) error {
	switch language {
	case "python":
		return validateByPatterns(code, pythonDangerousPatterns, caps)
	case "bash", "sh":
		return validateByPatterns(code, bashDangerousPatterns, caps)
	case "wasm":
		return ga.ValidateWasmImports(code, caps)
	case "typescript", "ts", "javascript", "js":
		return validateByPatterns(code, typescriptDangerousPatterns, caps)
	default:
		// 未知语言：记录日志，不拦截（不能假设恶意）
		slog.Warn("code_validator: unknown language, skipping", "language", language)
		return nil
	}
}

// wasiAllowedImports 始终允许的 WASI 导入（无需能力）。
var wasiAllowedImports = map[string]bool{
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
}

// wasiCapabilityGatedImports 需要特定能力才允许的 WASI 导入。
var wasiCapabilityGatedImports = map[string]string{
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
}

// readU32LEB128 手动解析 LEB128 的辅助函数
func readU32LEB128(data []byte, offset int) (uint32, int, error) {
	var result uint32
	var shift uint
	for i := offset; i < len(data); i++ {
		b := data[i]
		result |= uint32(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return result, i + 1, nil
		}
	}
	return 0, offset, perrors.New(perrors.CodeInvalidInput, "LEB128 decoding error")
}

func skipKindData(kind byte, wasmBytes []byte, importOffset int) (int, error) {
	switch kind {
	case 0: // func
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		return nextOffset, err
	case 1: // table
		importOffset++ // ref_type
		limitsFlag := wasmBytes[importOffset]
		importOffset++
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return 0, err
		}
		if limitsFlag == 1 {
			_, nextOffset, err = readU32LEB128(wasmBytes, nextOffset)
			return nextOffset, err
		}
		return nextOffset, nil
	case 2: // mem
		limitsFlag := wasmBytes[importOffset]
		importOffset++
		_, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return 0, err
		}
		if limitsFlag == 1 {
			_, nextOffset, err = readU32LEB128(wasmBytes, nextOffset)
			return nextOffset, err
		}
		return nextOffset, nil
	case 3: // global
		return importOffset + 2, nil
	}
	return importOffset, nil
}

func (ga *GovernanceAgent) validateImportSection(wasmBytes []byte, importOffset int, caps CapabilitySet) error {
	importCount, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
	if err != nil {
		return err
	}
	importOffset = nextOffset

	for i := uint32(0); i < importCount; i++ {
		modNameLen, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return err
		}
		importOffset = nextOffset
		modName := string(wasmBytes[importOffset : importOffset+int(modNameLen)])
		importOffset += int(modNameLen)

		fieldNameLen, nextOffset, err := readU32LEB128(wasmBytes, importOffset)
		if err != nil {
			return err
		}
		importOffset = nextOffset
		fieldName := string(wasmBytes[importOffset : importOffset+int(fieldNameLen)])
		importOffset += int(fieldNameLen)

		kind := wasmBytes[importOffset]
		importOffset++

		importOffset, err = skipKindData(kind, wasmBytes, importOffset)
		if err != nil {
			return err
		}

		key := modName + ":" + fieldName

		if wasiAllowedImports[key] {
			continue
		}

		if requiredCap, ok := wasiCapabilityGatedImports[key]; ok {
			if caps != nil && caps[requiredCap] {
				continue
			}
			return perrors.New(perrors.CodeForbidden, fmt.Sprintf("wasm import %s requires capability %s", key, requiredCap))
		}

		return perrors.New(perrors.CodeForbidden, fmt.Sprintf("wasm import %s is not allowed by policy", key))
	}
	return nil
}

// ValidateWasmImports 解析 Wasm 二进制的 Import Section，
// 拒绝导入了白名单之外宿主函数的 Wasm 模块。
func (ga *GovernanceAgent) ValidateWasmImports(wasmBytes []byte, caps CapabilitySet) error {
	if len(wasmBytes) < 8 {
		return perrors.New(perrors.CodeInvalidInput, "invalid wasm file")
	}

	magic := []byte{0x00, 0x61, 0x73, 0x6d}
	version := []byte{0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(wasmBytes[0:4], magic) || !bytes.Equal(wasmBytes[4:8], version) {
		return perrors.New(perrors.CodeInvalidInput, "invalid wasm magic/version")
	}

	offset := 8
	for offset < len(wasmBytes) {
		sectionID := wasmBytes[offset]
		offset++

		sectionSize, nextOffset, err := readU32LEB128(wasmBytes, offset)
		if err != nil {
			return err
		}
		offset = nextOffset

		if sectionID == 2 { // Import Section
			return ga.validateImportSection(wasmBytes, offset, caps)
		}

		offset += int(sectionSize)
	}

	return nil
}

// typescriptDangerousPatterns TypeScript/JavaScript 危险调用规则表。
// 设计说明：这是第一道静态防线（正则），覆盖"明显恶意"模式。
// 运行时第二道防线由 Deno 权限标志（capability flags）承担，见 plugin_creator.go。
var typescriptDangerousPatterns = []codeRule{
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
}

// dangerousGoPackages 未持有对应 Capability 时禁止导入的包。
var dangerousGoPackages = map[string]string{
	"os/exec":       "shell_exec",
	"syscall":       "shell_exec",
	"unsafe":        "native_memory",
	"net":           "network_raw",
	"net/http":      "network_raw",
	"crypto/tls":    "network_raw",
	"plugin":        "shell_exec",
	"runtime/debug": "native_memory",
}

// auditGoAST 解析 Go 源码 AST，拦截未授权包导入。
// 仅扫描 import 声明，O(imports) 复杂度，不做全量 AST 遍历。
func auditGoAST(code []byte, caps CapabilitySet) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", code, parser.ImportsOnly)
	if err != nil {
		// 解析失败 = 语法错误代码，阻断执行
		return perrors.New(perrors.CodeForbidden, "go AST parse failed: "+err.Error())
	}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		// 去除引号
		pkg := strings.Trim(imp.Path.Value, `"`)
		if requiredCap, blocked := dangerousGoPackages[pkg]; blocked {
			if !caps[requiredCap] {
				return perrors.New(perrors.CodeForbidden,
					fmt.Sprintf("AST: unauthorized import %q requires capability %q", pkg, requiredCap))
			}
		}
	}
	return nil
}

// pythonDangerousImports Python 禁止导入映射（import 行扫描，不用正则全文匹配）。
var pythonDangerousImports = map[string]string{
	"subprocess": "shell_exec",
	"os":         "shell_exec", // os.system / os.popen
	"pty":        "shell_exec",
	"socket":     "network_raw",
	"urllib":     "network_raw",
	"requests":   "network_raw",
	"ctypes":     "native_memory",
	"cffi":       "native_memory",
}

// bashDangerousCommands Bash 禁止命令映射（行首匹配）。
var bashDangerousCommands = map[string]string{
	"curl ":  "network_raw",
	"wget ":  "network_raw",
	"nc ":    "network_raw",
	"ncat ":  "network_raw",
	"eval ":  "shell_exec",
	"exec ":  "shell_exec",
	"chmod ": "shell_exec",
	"chown ": "shell_exec",
}

// tsDangerousImports TypeScript/JavaScript 禁止导入映射。
var tsDangerousImports = map[string]string{
	"child_process":  "shell_exec",
	"fs":             "shell_exec",
	"net":            "network_raw",
	"http":           "network_raw",
	"https":          "network_raw",
	"vm":             "shell_exec",
	"worker_threads": "shell_exec",
}

// auditImportLines 扫描代码每行，检测危险 import/require 语句。
// 不做完整 AST 解析，仅匹配 import/from/require 行，性能 O(lines)。
func auditImportLines(code []byte, dangerousMap map[string]string, caps CapabilitySet) error {
	for _, line := range strings.Split(string(code), "\n") {
		trimmed := strings.TrimSpace(line)
		// 跳过注释行
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		for keyword, requiredCap := range dangerousMap {
			if strings.Contains(trimmed, keyword) {
				if !caps[requiredCap] {
					return perrors.New(perrors.CodeForbidden,
						fmt.Sprintf("AST: dangerous import/use of %q requires capability %q", keyword, requiredCap))
				}
			}
		}
	}
	return nil
}
