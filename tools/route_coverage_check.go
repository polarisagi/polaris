//go:build ignore

// route_coverage_check 检查 gateway Handler 方法与 server_routes.go 路由注册对账（F-8a）。
//
// 防御目标：
//   1. 阻止 Handler 已实现但路由被遗漏/注释挂起（如提示词路由 C-1 / VFSUpload C-3 潜伏案件）
//   2. 报告 server_routes.go 中被注释掉的 `// mux.Handle(` / `// mux.HandleFunc(`
//
// 豁免：对于明确待接线前置条件的 handler，可以在注释/豁免名单 `tools/route-coverage-allowlist.txt` 标记。
//
// 使用：
//	go run tools/route_coverage_check.go
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var errCount int
var handlerCount int

func main() {
	routesFile := "internal/gateway/server/server_routes.go"
	allowlistPath := "tools/route-coverage-allowlist.txt"
	allowlist, _ := loadAllowlist(allowlistPath)

	// 1. 检查 server_routes.go 中是否存在被注释的路由
	checkCommentedRoutes(routesFile, allowlist)

	// 2. 收集已在 server_routes.go (及关联路由文件) 注册的方法名
	registeredMethods := collectRegisteredMethods("internal/gateway/server")

	// 3. 扫描 internal/gateway/server/ 下全部 HTTP Handler 方法
	fset := token.NewFileSet()
	filepath.Walk("internal/gateway/server", func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checkHandlerFile(fset, path, registeredMethods, allowlist)
		return nil
	})

	fmt.Printf("route_coverage_check: scanned %d HTTP Handler method(s)\n", handlerCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "route_coverage_check: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("route_coverage_check: PASS")
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
		list[line] = true
	}
	return list, scanner.Err()
}

func checkCommentedRoutes(path string, allowlist map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") && (strings.Contains(trimmed, "mux.Handle(") || strings.Contains(trimmed, "mux.HandleFunc(")) {
			// 如果注释中包含的 handler 在 allowlist 中，跳过
			isAllowed := false
			for allowed := range allowlist {
				if strings.Contains(trimmed, allowed) {
					isAllowed = true
					break
				}
			}
			if !isAllowed {
				fmt.Printf("%s:%d: 存在被注释的路由注册 %q（违反 F-8a，未完成接线禁止使用注释挂起）\n",
					path, i+1, trimmed)
				errCount++
			}
		}
	}
}

func collectRegisteredMethods(dir string) map[string]bool {
	registered := make(map[string]bool)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// 匹配 handler.HandleXxx 方法引用
			if strings.Contains(trimmed, ".Handle") {
				idx := strings.Index(trimmed, ".Handle")
				sub := trimmed[idx+1:]
				// 提取标识符
				end := 0
				for end < len(sub) && ((sub[end] >= 'a' && sub[end] <= 'z') || (sub[end] >= 'A' && sub[end] <= 'Z') || (sub[end] >= '0' && sub[end] <= '9') || sub[end] == '_') {
					end++
				}
				if end > 0 {
					registered[sub[:end]] = true
				}
			}
		}
		return nil
	})
	return registered
}

func checkHandlerFile(fset *token.FileSet, path string, registered map[string]bool, allowlist map[string]bool) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() {
			return true
		}

		// 检查签名：func (h *XxxHandler) HandleXxx(w http.ResponseWriter, r *http.Request)
		if !strings.HasPrefix(fn.Name.Name, "Handle") {
			return true
		}

		if isHTTPHandlerSignature(fn) {
			handlerCount++
			methodName := fn.Name.Name
			key := fmt.Sprintf("%s:%s", path, methodName)
			if !registered[methodName] && !allowlist[key] && !allowlist[methodName] {
				pos := fset.Position(fn.Pos())
				fmt.Printf("%s:%d: HTTP Handler 方法 %q 未在 server_routes.go 中注册（违反 F-8a 路由接线自洽性）\n",
					path, pos.Line, methodName)
				errCount++
			}
		}
		return true
	})
}

func isHTTPHandlerSignature(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 2 {
		return false
	}
	// 简单断言有两个参数
	return true
}
