//go:build ignore

// memory_isolation_check —— [L-18] 情景记忆项目隔离门控（ADR-0097 决策三修订）。
//
// 判据是**可实跑的跨项目泄漏用例**，不是静态扫描：项目 A 写入唯一标记串，项目 B 经
// 读取面 P1~P6 各自查询都不得命中（用例名前缀 TestProjectIsolation）。
//
// 为什么不用静态规则："每条召回路都调用了过滤函数"可以被静态校验，但它只能证明
// "话说了"——过滤函数判错归属、Source 分类漏一种、包装层被旁路，静态规则全部看不见。
//
// 防空转：只看 go test 退出码不够。用例被改名/删除时 `-run` 匹配不到任何测试，go test
// 照样返回 0（"no tests to run"）。故逐包要求至少一条 PASS，并对全体设最低条数。
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// isolationPackages 读取面与其用例所在包（ADR-0097 决策三修订"读取面清单"）。
var isolationPackages = []string{ //nolint:gochecknoglobals // 工具脚本内的只读清单
	"./internal/memory/store/",     // P1/P2/P5 底层过滤点 EpisodicQuery.ProjectID；写入打标；Durative 分桶
	"./internal/memory/retrieval/", // P6 HybridRetriever 七路（包装层 + Tier0/Tier1 端到端）
	"./internal/agent/context/",    // P1~P4 感知/规划上下文组装
	"./internal/tool/builtin/",     // P6 入口：memory_search 工具取项目作用域
	"./cmd/polaris/",               // P5 Assembler 情景适配器
}

// minPassing 全体最少通过条数：低于它说明有用例被删/改名而门控未同步。
const minPassing = 11

func main() {
	args := append([]string{"test", "-count=1", "-v", "-run", "^TestProjectIsolation"}, isolationPackages...)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS=", "GOARCH=")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	passed := map[string]int{} // 包 → PASS 条数
	total := 0
	var failures []string
	pending := 0
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "--- PASS: TestProjectIsolation"):
			pending++
			total++
		case strings.HasPrefix(line, "--- FAIL:"), strings.Contains(line, "_test.go:"):
			failures = append(failures, line)
		case strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "ok \t"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				passed[fields[1]] += pending
			}
			pending = 0
		}
	}

	fmt.Println("=== [L-18] 情景记忆项目隔离门控（ADR-0097 决策三修订）===")
	if runErr != nil {
		fmt.Println("memory-isolation-check: FAIL —— 跨项目泄漏用例未通过：")
		for _, f := range failures {
			fmt.Println("  " + f)
		}
		if len(failures) == 0 {
			fmt.Println(out.String())
		}
		os.Exit(1)
	}
	for _, pkg := range isolationPackages {
		key := "github.com/polarisagi/polaris/" + strings.Trim(strings.TrimPrefix(pkg, "./"), "/")
		if passed[key] == 0 {
			fmt.Printf("memory-isolation-check: FAIL —— %s 没有任何 TestProjectIsolation 用例运行（被删除或改名？）\n", pkg)
			os.Exit(1)
		}
	}
	if total < minPassing {
		fmt.Printf("memory-isolation-check: FAIL —— 仅 %d 条用例通过，低于下限 %d（用例被删减而门控未同步）\n", total, minPassing)
		os.Exit(1)
	}
	fmt.Printf("memory-isolation-check: PASS（%d 个包，%d 条跨项目泄漏用例）\n", len(isolationPackages), total)
}
