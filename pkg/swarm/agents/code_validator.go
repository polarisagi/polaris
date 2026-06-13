package agents

import (
	"bytes"
	"fmt"
	"log/slog"
	"regexp"

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
