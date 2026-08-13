//go:build ignore

// ffi_symbol_check 检查 Rust 侧 C-ABI 导出符号与 Go 侧 purego/FFI 注册符号的双向对账（F-8b）。
//
// 规则：
//   - S - G 非空（Rust 导出了但 Go 未绑定）：报告可能遗漏的死符号，或需登记在 scripts/deadcode-allowlist.txt
//   - G - S 非空（Go 绑定了 Rust 不存在的符号）：运行时必然 panic/crash，强行报错
//
// 使用：
//
//	go run tools/ffi_symbol_check.go
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var errCount int

func main() {
	rustDir := "rust/substrate/src"
	goDirs := []string{"internal", "cmd", "pkg"}
	allowlistPath := "scripts/deadcode-allowlist.txt"

	allowlist, _ := loadAllowlist(allowlistPath)

	// 1. 从 Rust 代码提取 pub extern "C" fn <name>
	rustSymbols := collectRustSymbols(rustDir)

	// 2. 从 Go 代码提取 purego.RegisterLibFunc(..., "<name>") 及 FFI 符号
	goSymbols := collectGoSymbols(goDirs)

	fmt.Printf("ffi_symbol_check: found %d Rust C-FFI symbol(s), %d Go FFI binding(s)\n",
		len(rustSymbols), len(goSymbols))

	// 自检：Go 绑定数为 0 说明提取正则与实际调用形态失配，此时 S-G 会把全部
	// Rust 符号误报为未绑定，白名单一加就「绿了」。这种恒绿门控与没有门控在 CI
	// 输出上完全一样，必须直接判失败（2026-08-12 实测踩过一次）。
	if len(rustSymbols) > 0 && len(goSymbols) == 0 {
		fmt.Fprintln(os.Stderr,
			"ffi_symbol_check: FAIL — Go 侧提取到 0 个 FFI 绑定，提取正则与实际调用形态失配；"+
				"在修好提取逻辑之前本门控的结论一律不可信，禁止用白名单消化其输出")
		os.Exit(1)
	}

	// 检查 Rust 有但 Go 未绑定 (S - G)
	for sym, loc := range rustSymbols {
		if goSymbols[sym] == "" && !allowlist[sym] {
			fmt.Printf("%s: Rust 导出了 C-FFI 符号 %q 但 Go 侧未绑定且未登记在 deadcode-allowlist.txt（违反 F-8b FFI 符号对账约束）\n",
				loc, sym)
			errCount++
		}
	}

	// 检查 Go 绑定了但 Rust 不存在 (G - S)
	//
	// 只对 substrate 绑定点生效：仓库里还有 sherpa-onnx 等第三方 dylib 的
	// purego 绑定（internal/llm/stt|tts/sherpa.go），它们的符号本就不该出现在
	// rust/substrate 里，拿 substrate 的符号表去判它们是量错了尺子。
	for sym, loc := range goSymbols {
		if !isSubstrateBindingSite(loc) {
			continue
		}
		if len(rustSymbols) > 0 && rustSymbols[sym] == "" {
			fmt.Printf("%s: Go 绑定了 Rust 不存在的 FFI 符号 %q（运行时必定失败，违反 F-8b）\n",
				loc, sym)
			errCount++
		}
	}

	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "ffi_symbol_check: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("ffi_symbol_check: PASS")
}

func loadAllowlist(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	list := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 支持单符号行 "symbol_name" 或 deadcode 格式 "<file>: unreachable func: <symbol_name> # <comment>"
		if strings.Contains(line, "unreachable func: ") {
			parts := strings.Split(line, "unreachable func: ")
			if len(parts) > 1 {
				symPart := strings.TrimSpace(parts[1])
				if idx := strings.Index(symPart, " "); idx > 0 {
					symPart = symPart[:idx]
				}
				list[symPart] = true
			}
		} else {
			if idx := strings.Index(line, " "); idx > 0 {
				line = line[:idx]
			}
			list[line] = true
		}
	}
	return list, scanner.Err()
}

// isSubstrateBindingSite 判定该 Go 文件绑定的是不是 rust/substrate 那一个 dylib。
// 判据：文件位于 internal/ffi/（substrate dylib 的加载与绑定归口），或显式 import
// 了 internal/ffi（经由 ffi.Dylib 拿到 substrate handle 再自行 RegisterLibFunc）。
// 其余 purego 绑定点（sherpa-onnx 等第三方库）不参与 substrate 符号表对账。
func isSubstrateBindingSite(path string) bool {
	norm := filepath.ToSlash(path)
	if strings.HasPrefix(norm, "internal/ffi/") {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `polarisagi/polaris/internal/ffi"`)
}

func collectRustSymbols(dir string) map[string]string {
	symbols := make(map[string]string)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return symbols
	}

	// pub [unsafe] extern "C" fn <name>
	fnRe := regexp.MustCompile(`pub\s+(?:unsafe\s+)?extern\s+"C"\s+fn\s+([a-zA-Z0-9_]+)`)

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".rs") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		matches := fnRe.FindAllStringSubmatch(string(data), -1)
		for _, m := range matches {
			if len(m) > 1 {
				symbols[m[1]] = path
			}
		}
		return nil
	})
	return symbols
}

func collectGoSymbols(dirs []string) map[string]string {
	symbols := make(map[string]string)
	// purego 的实际签名是三参：RegisterLibFunc(fptr, handle, name) /
	// RegisterFunc(fptr, cfn)。2026-08-12 复核：原正则写成两参
	// `\([^,]+,\s*"..."`，与三参调用形态永不匹配，导致本门控恒返回
	// 「0 Go FFI binding(s)」——46 个 Rust 符号被全量误报为未绑定，随后被整批
	// 塞进 deadcode-allowlist.txt 掩盖。门控扫描数为 0 就是门控没在工作
	// （ADR-0091：看门控在看哪里，比看门控报了什么更重要）。
	regRe := regexp.MustCompile(`Register(?:Lib)?Func\(\s*&?[A-Za-z0-9_.\[\]]+\s*,\s*[A-Za-z0-9_.]+\s*,\s*"([a-zA-Z0-9_]+)"`)
	// purego.Dlsym(lib, "symbol") 直接取符号地址的形态同样算绑定。
	dlsymRe := regexp.MustCompile(`Dlsym\(\s*[A-Za-z0-9_.]+\s*,\s*"([a-zA-Z0-9_]+)"`)

	for _, dir := range dirs {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, re := range []*regexp.Regexp{regRe, dlsymRe} {
				for _, m := range re.FindAllStringSubmatch(string(data), -1) {
					if len(m) > 1 {
						symbols[m[1]] = path
					}
				}
			}
			return nil
		})
	}
	return symbols
}
