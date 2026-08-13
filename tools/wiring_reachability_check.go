//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func main() {
	cmd := exec.Command("go", "run", "golang.org/x/tools/cmd/deadcode@latest", "./cmd/polaris/...")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := out.String()

	if err != nil && !strings.Contains(output, "unreachable") && !strings.Contains(output, "deadcode:") {
		// If it's a compile error, we just print it and fail
		fmt.Fprintf(os.Stderr, "deadcode failed: %v\n%s\n", err, output)
		os.Exit(2)
	}

	allowlistMap := make(map[string]bool)
	allowlistBytes, err := os.ReadFile("scripts/wiring-allowlist.txt")
	if err == nil {
		lines := strings.Split(string(allowlistBytes), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "#") && line != "" {
				// e.g. "internal/xxx.go:Func" or just "internal/xxx.go"
				allowlistMap[line] = true
			}
		}
	}

	hasError := false

	lines := strings.Split(output, "\n")
	// deadcode output format:
	// path/to/file.go:line:col: unreachable func: FuncName

	funcRe := regexp.MustCompile(`^(.*internal/.*\.go):\d+:\d+:\s*(?:unreachable func:\s*([A-Z]\w*)|.*)$`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if !strings.Contains(line, "internal/") {
			continue
		}

		m := funcRe.FindStringSubmatch(line)
		if m == nil || m[2] == "" {
			continue
		}

		filePath := m[1]
		// extract relative path
		idx := strings.Index(filePath, "internal/")
		if idx != -1 {
			filePath = filePath[idx:]
		}

		funcName := m[2]

		// Only care about exported functions
		if !isExported(funcName) {
			continue
		}

		matchKey := fmt.Sprintf("%s:%s", filePath, funcName)
		if allowlistMap[matchKey] || allowlistMap[filePath] {
			continue
		}

		fmt.Fprintf(os.Stderr, "%s: 导出函数 %s 不可达（未接线，违反 L-13）\n", filePath, funcName)
		hasError = true
	}

	if hasError {
		os.Exit(1)
	}
	fmt.Println("wiring-reachability-check ok")
}

func isExported(name string) bool {
	if name == "" {
		return false
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}
