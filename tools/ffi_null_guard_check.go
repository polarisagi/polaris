//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var funcDeclRe = regexp.MustCompile(`(?s)pub\s+unsafe\s+extern\s+"C"\s+fn\s+([a-zA-Z0-9_]+)\s*\((.*?)\)(?:\s*->\s*[^{]*?)?\s*\{`)

// paramRe 只匹配**入参**指针（`*const T`）。
//
// 出参（`*mut T` / `*mut *mut T`）刻意不纳入判定：它们的写入统一走
// write_cstr / write_bytes / write_err 系列助手，助手自身已做 null 判定
// （见 rust/substrate/src/lib.rs write_bytes、surreal_store/mod.rs write_cstr），
// 且本 ABI 在多处 doc 注释里把 `out_* 可为 null` 写成契约。若把出参也要求
// 逐函数守卫，会诱导实现方给它们加 `return -1` 早退——那正是 2026-08-13 轮把
// wasmtime/llama_infer/native_sandbox 的合法调用打成硬失败的成因。
var paramRe = regexp.MustCompile(`([a-zA-Z0-9_]+)\s*:\s*\*const\s+[a-zA-Z0-9_]+`)

// nullSafeAccessors 是已自带 NULL 判定的入参读取助手。
// 参数只要全程经由它们取值，就无需在函数体内再写一次 `x.is_null()`——
// 重复守卫不增加安全性，却会把「NULL = 缺省/空值」这类文档化哨兵语义
// 误改成硬失败。新增同类助手时必须同步登记到本表。
var nullSafeAccessors = []string{
	"slice_to_str(",     // lib.rs：ptr==null || len==0 → Ok("")
	"slice_to_str_mut(", // 同上，可变版本
	"li_read_cstr(",     // llama_infer/dispatch.rs：ptr==null → Ok("")
	"ns_read_cstr(",     // native_sandbox/engine.rs：ptr==null → Ok("")
}

func main() {
	var rsFiles []string
	err := filepath.Walk("rust/substrate/src", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".rs") {
			rsFiles = append(rsFiles, path)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Walk failed: %v\n", err)
		os.Exit(2)
	}

	hasError := false
	for _, path := range rsFiles {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		strContent := string(content)

		matches := funcDeclRe.FindAllStringSubmatchIndex(strContent, -1)
		for i, m := range matches {
			funcName := strContent[m[2]:m[3]]
			paramsStr := strContent[m[4]:m[5]]

			startIdx := m[1]
			endIdx := len(strContent)
			if i+1 < len(matches) {
				endIdx = matches[i+1][0]
			}
			funcBody := strContent[startIdx:endIdx]

			paramMatches := paramRe.FindAllStringSubmatch(paramsStr, -1)
			for _, pm := range paramMatches {
				paramName := pm[1]

				// 两条通过判据（满足其一即可）：
				//   1. 函数体内显式写了 `<param>.is_null()`——无论用于早退还是分支处理；
				//   2. 该参数全程经由 nullSafeAccessors 中的助手取值。
				guarded := strings.Contains(funcBody, paramName+".is_null()")
				for _, acc := range nullSafeAccessors {
					if strings.Contains(funcBody, acc+paramName) {
						guarded = true
						break
					}
				}

				if !guarded {
					fmt.Fprintf(os.Stderr, "%s: 导出函数 %s 参数 %s 缺少 NULL 守卫，违反 FFI 安全边界要求（L-08）\n", path, funcName, paramName)
					hasError = true
				}
			}
		}
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("ffi-null-guard-check ok")
}
