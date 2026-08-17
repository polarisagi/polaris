//go:build ignore

// task_state_lint 检查 tasks 表的 CAS 状态转移 SQL 是否符合 inv_M8_03 状态机定义（F-5），以及 outbox_state_lint 检查。
//
// inv_M8_03 合法状态转移：
//
//	pending  -> claimed
//	claimed  -> running | failed
//	running  -> done | failed
//
// 违规：允许 claimed 直接跳到 done（绕过 running 导致认领超时乱序）。
//
// 使用：
//
//	go run tools/task_state_lint.go
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var errCount int
var queryCount int

func main() {
	targetDirs := []string{"internal/execute/orchestrator", "internal/store/repo"}
	fset := token.NewFileSet()

	for _, dir := range targetDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkFile(fset, path)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "task_state_lint: walk %s: %v\n", dir, err)
			os.Exit(2)
		}
	}

	checkOutboxWorker(fset)

	fmt.Printf("task_state_lint: scanned %d task state update query(ies)\n", queryCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "task_state_lint: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("task_state_lint: PASS")
}

// checkFile 在整个调用表达式上做判定，而不是只看 SQL 字符串字面量。
//
// 2026-08-12 修正：原实现只 Inspect *ast.BasicLit，然后在 SQL 文本里找
// "statusclaimed" / "statusrunning" 这些**Go 标识符**。但状态常量是
// ExecContext 的实参、从不出现在 SQL 文本里（SQL 里只有 `status IN (?,?)`
// 这样的占位符），条件恒不成立——该规则自诞生起从未报过一次红。
// 经 make lint-selftest 负向验证暴露。
//
// 正确形态：找到实参里含 "update tasks ... status" 的调用，再看同一调用的
// 其余实参用了哪些状态常量，据此判定这条 CAS 允许的源状态集合。
func checkFile(fset *token.FileSet, path string) {
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return
	}

	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sqlIdx, sqlText := findTasksUpdateSQL(call.Args)
		if sqlIdx < 0 {
			return true
		}
		queryCount++

		// 收集本次调用用到的状态常量标识符（跳过 SQL 字面量本身）。
		states := map[string]bool{}
		for i, arg := range call.Args {
			if i == sqlIdx {
				continue
			}
			if id, ok := arg.(*ast.Ident); ok {
				states[id.Name] = true
			}
		}

		// 只约束「成功完成」这一条边：done 必须来自 running。
		//
		// 不约束 → failed：失败/取消是中止路径，从 pending / claimed / running 任一
		// 非终态发起都合法（认领后未启动就崩溃、租约过期、Saga 取消），强行要求先经
		// running 会逼出一次假的状态推进。这与 inv_M8_03 的转移表一致：
		//   pending -> claimed;  claimed -> running | failed;  running -> done | failed
		//
		// 2026-08-12：本条判据最初写成「终态一律只许来自 running」，一上线就把
		// FailTask 与 CancelTask 两条合法中止路径判成违规——过严的门控最终会被整体
		// 关掉，比漏报更糟，故收窄到 done。
		if states["statusDone"] && states["statusClaimed"] {
			pos := fset.Position(call.Pos())
			fmt.Printf("%s:%d: CAS 允许 claimed 跳过 running 直达 done（违反 inv_M8_03 F-5）；SQL: %s\n",
				path, pos.Line, condenseSQL(sqlText))
			errCount++
		}
		return true
	})
}

// findTasksUpdateSQL 返回实参中 tasks 表 UPDATE 语句的下标与原文，未命中返回 -1。
func findTasksUpdateSQL(args []ast.Expr) (int, string) {
	for i, arg := range args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		low := strings.ToLower(lit.Value)
		if strings.Contains(low, "update tasks") && strings.Contains(low, "status") {
			return i, lit.Value
		}
	}
	return -1, ""
}

// condenseSQL 把多行 SQL 压成一行，便于在报错里一眼看清 WHERE 条件。
func condenseSQL(s string) string {
	s = strings.NewReplacer("\n", " ", "\t", " ", "`", "").Replace(s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// checkOutboxWorker 断言 L-06：毒丸分支（CrashRecoveryCount 超阈）不得 return nil，
// 否则崩溃恢复计数用尽的消息会被当作处理成功而丢弃。
//
// 2026-08-17 两处修正：
//
//  1. 原实现在文件打不开/解析失败时直接 `return`，静默当作通过。锚点消失必须表现为
//     红灯——这正是 ADR-0091 那四种门控失真形态里的第一种（豁免自己）。改为 exit 2。
//  2. 原实现要求阈值是**字面量 3**（`bin.Y` 必须是 BasicLit "3"）。把 3 提成命名常量
//     是一次纯粹的良性重构，却会让本规则从此一无所获且继续打印 PASS。判据改为
//     「CrashRecoveryCount 与任何东西比较」，不再绑死右操作数的形态。
func checkOutboxWorker(fset *token.FileSet) {
	path := "internal/store/outbox_worker.go"
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "task_state_lint: FAIL — 无法解析 L-06 的锚点文件 %s：%v。"+
			"文件被移动或改名时必须同步本规则，而不是让门控静默通过\n", path, err)
		os.Exit(2)
	}

	anchorFound := false

	ast.Inspect(node, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}

		// 找 CrashRecoveryCount 的上界比较；右操作数是字面量还是命名常量都算。
		isCrashCountCheck := false
		ast.Inspect(ifStmt.Cond, func(cn ast.Node) bool {
			bin, ok := cn.(*ast.BinaryExpr)
			if !ok {
				return true
			}
			if bin.Op != token.GEQ && bin.Op != token.GTR {
				return true
			}
			if id, ok := bin.X.(*ast.SelectorExpr); ok {
				if id.Sel.Name == "CrashRecoveryCount" {
					isCrashCountCheck = true
				}
			} else if id, ok := bin.X.(*ast.Ident); ok {
				if strings.Contains(id.Name, "CrashRecovery") {
					isCrashCountCheck = true
				}
			}
			return true
		})

		if isCrashCountCheck {
			anchorFound = true
			// Ensure it does not return nil
			ast.Inspect(ifStmt.Body, func(bn ast.Node) bool {
				ret, ok := bn.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, res := range ret.Results {
					if id, ok := res.(*ast.Ident); ok && id.Name == "nil" {
						pos := fset.Position(ret.Pos())
						fmt.Printf("%s:%d: CrashRecoveryCount 超阈的毒丸分支 return nil，消息会被当作处理成功而丢弃"+
							"（违反 outbox_inv_03 / L-06）\n", path, pos.Line)
						errCount++
					}
				}
				return true
			})
		}
		return true
	})

	if !anchorFound {
		fmt.Fprintf(os.Stderr, "task_state_lint: FAIL — 在 %s 里找不到任何 CrashRecoveryCount 上界判定。"+
			"L-06 的判据锚在这个比较上，字段改名或毒丸逻辑被移走都会走到这里；"+
			"请同步本规则而非让它继续静默通过（exit 2）\n", path)
		os.Exit(2)
	}
}
