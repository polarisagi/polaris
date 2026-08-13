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
var paramRe = regexp.MustCompile(`([a-zA-Z0-9_]+)\s*:\s*(?:mut\s+)?\*(?:const|mut)\s+[a-zA-Z0-9_]+`)

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

				// check if is_null() or slice_to_str is used with this param
				isNullCheck := strings.Contains(funcBody, paramName+".is_null()")
				sliceToStrCheck := strings.Contains(funcBody, "slice_to_str("+paramName)
				sliceToStrCheck2 := strings.Contains(funcBody, "slice_to_str_mut("+paramName)

				if !isNullCheck && !sliceToStrCheck && !sliceToStrCheck2 {
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
