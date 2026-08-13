//go:build ignore

// wiring_reachability_check.go — [L-13] 包级接线可达性门控。
//
// 立此门控的原因（ADR-0091：先看门控在看哪里）：
// 既有 `make deadcode` 走 golang.org/x/tools 的 deadcode，判据是**函数级**可达性，
// 且以整个 module 为根——一个包只要被单测引用，其符号就"可达"，包本身是否被生产
// 入口接线它看不出来。`internal/cli`（AgentREPL/RateLimiterMiddleware/WebSocketHub，
// 全仓零生产 import）正是这样长期逃逸的。
//
// 本门控只回答 deadcode 回答不了的那一个问题：**从生产入口 cmd/polaris 出发，
// 这个 internal 包在不在传递 import 闭包里**。包级判据，与符号级判据互补不重叠。
//
// 实现刻意用 `go list`（工具链自带、离线、结果确定）而不是再跑一遍 deadcode：
//   - 初版实现 `go run golang.org/x/tools/cmd/deadcode@latest`，在 lint 期联网拉取
//     未固定版本，离线 CI 直接失败，且与 make deadcode 完全重复；
//   - 那版的正则 `unreachable func:\s*([A-Z]\w*)` 匹配不到方法（deadcode 输出形如
//     `Tracer.RegisterExporter`，`\w` 不含 `.`，回溯到 `.*` 分支后捕获组为空被跳过），
//     而 L-13 要覆盖的 6 条 GR 全是方法——结构上抓不到它声称要防的东西。
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	modulePath    = "github.com/polarisagi/polaris"
	allowlistPath = "scripts/wiring-allowlist.txt"
)

func main() {
	reachable, err := listPackages("-deps", "./cmd/polaris/...")
	if err != nil {
		fmt.Fprintf(os.Stderr, "wiring_reachability_check: 解析生产入口依赖闭包失败: %v\n", err)
		os.Exit(2)
	}
	all, err := listPackages("./internal/...")
	if err != nil {
		fmt.Fprintf(os.Stderr, "wiring_reachability_check: 枚举 internal 包失败: %v\n", err)
		os.Exit(2)
	}

	allow, err := loadAllowlist(allowlistPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wiring_reachability_check: 读取白名单失败: %v\n", err)
		os.Exit(2)
	}

	reachableSet := make(map[string]bool, len(reachable))
	for _, p := range reachable {
		reachableSet[p] = true
	}

	var orphans []string
	for _, pkg := range all {
		if reachableSet[pkg] {
			continue
		}
		rel := strings.TrimPrefix(pkg, modulePath+"/")
		if allow[rel] {
			continue
		}
		orphans = append(orphans, rel)
	}

	for _, o := range orphans {
		fmt.Fprintf(os.Stderr,
			"%s: 该包不在 cmd/polaris 的生产 import 闭包内（零接线，违反 L-13）。"+
				"处置只有三条：接线 / 删除 / 逐条登记 %s 并写明「为何暂不接线 + 去向」\n",
			o, allowlistPath)
	}
	if len(orphans) > 0 {
		os.Exit(1)
	}

	fmt.Printf("wiring-reachability-check ok（生产闭包 %d 包，internal %d 包，白名单 %d 条）\n",
		len(reachable), len(all), len(allow))
}

// listPackages 调用 go list 返回包导入路径列表，只保留本 module 内的包。
func listPackages(args ...string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list", "-f", "{{.ImportPath}}"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go list %v: %w\n%s", args, err, out)
	}
	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modulePath) {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs, nil
}

// loadAllowlist 读取白名单。每行一个包相对路径（如 internal/cli），# 开头为注释。
func loadAllowlist(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer f.Close()

	allow := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// 行内注释：豁免条目要求写明理由，理由就跟在同一行的 # 之后。
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 兼容旧格式 `path:Symbol`：本门控是包级判据，取冒号前的路径部分。
		if i := strings.Index(line, ":"); i > 0 {
			line = line[:i]
		}
		// 旧格式登记的是文件路径，取其所在目录作为包路径。
		if strings.HasSuffix(line, ".go") {
			if i := strings.LastIndex(line, "/"); i > 0 {
				line = line[:i]
			}
		}
		allow[line] = true
	}
	return allow, sc.Err()
}
