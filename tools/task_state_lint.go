//go:build ignore

// task_state_lint 守任务状态机与 outbox 的两条不变量：
//
//	[inv_M8_03] tasks 表的 CAS 状态转移 SQL 不得允许 claimed 跳过 running 直达 done（F-5）
//	[L-06]      outbox 毒丸分支（CrashRecoveryCount 超阈）不得 return nil
//
// inv_M8_03 合法状态转移：
//
//	pending  -> claimed
//	claimed  -> running | failed
//	running  -> done | failed
//
// 两条断言住在同一个二进制里，Makefile 也只挂一个目标（2026-08-17 合并；此前
// task-state-check 与 outbox-state-check 各挂一个且都在 lint 里，同一二进制跑两遍）。
// 门控条数按 [ID] 计而非按目标计，故 Makefile 侧为两条 ID 各留一行报头。
package main

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

// casDirs 是 tasks 表 CAS 语句的所在处。收窄到这两个目录是有据的：
// SQL 常量集中在 Repository 与 Blackboard 两层，扩到全仓只会把测试夹具卷进来。
var casDirs = []string{"internal/execute/orchestrator", "internal/store/repo"}

const outboxPath = "internal/store/outbox_worker.go"

func main() {
	r := lintutil.NewReporter("task-state-lint", nil) // fail-closed：状态机不接受存量

	lintutil.Walk(r, lintutil.WalkOptions{Roots: casDirs}, func(f lintutil.File) {
		checkCAS(r, f)
	})
	r.RequireAnchors(1, "inv_M8_03 的判据锚在含 `UPDATE tasks ... status` 的 SQL 字面量上；"+
		"一条都找不到说明 CAS 语句被移出 "+strings.Join(casDirs, " / "))

	checkOutboxWorker(r)
	r.Done()
}

// ─── inv_M8_03 ───────────────────────────────────────────────────────────────

// checkCAS 在整个调用表达式上做判定，而不是只看 SQL 字符串字面量。
//
// 2026-08-12 修正：原实现只 Inspect *ast.BasicLit，然后在 SQL 文本里找
// "statusclaimed" / "statusrunning" 这些 **Go 标识符**。但状态常量是 ExecContext 的
// 实参、从不出现在 SQL 文本里（SQL 里只有 `status IN (?,?)` 这样的占位符），
// 条件恒不成立——该规则自诞生起从未报过一次红。经 make lint-selftest 负向验证暴露。
//
// 正确形态：找到实参里含 "update tasks ... status" 的调用，再看同一调用的其余实参
// 用了哪些状态常量，据此判定这条 CAS 允许的源状态集合。
func checkCAS(r *lintutil.Reporter, f lintutil.File) {
	lintutil.Calls(f.AST, func(call *ast.CallExpr) {
		sqlIdx := findTasksUpdateSQL(call.Args)
		if sqlIdx < 0 {
			return
		}
		r.Anchor()

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
		// running 会逼出一次假的状态推进。这与 inv_M8_03 的转移表一致。
		//
		// 2026-08-12：本条判据最初写成「终态一律只许来自 running」，一上线就把
		// FailTask 与 CancelTask 两条合法中止路径判成违规——过严的门控最终会被整体
		// 关掉，比漏报更糟，故收窄到 done。
		if states["statusDone"] && states["statusClaimed"] {
			r.Violation(f.At(call), "CAS 允许 claimed 跳过 running 直达 done，"+
				"认领超时与完成会乱序（违反 inv_M8_03 / F-5）")
		}
	})
}

// findTasksUpdateSQL 返回实参中 tasks 表 UPDATE 语句的下标，未命中返回 -1。
func findTasksUpdateSQL(args []ast.Expr) int {
	for i, arg := range args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		low := strings.ToLower(lit.Value)
		if strings.Contains(low, "update tasks") && strings.Contains(low, "status") {
			return i
		}
	}
	return -1
}

// ─── L-06 ────────────────────────────────────────────────────────────────────

// checkOutboxWorker 断言毒丸分支（CrashRecoveryCount 超阈）不得 return nil，
// 否则崩溃恢复计数用尽的消息会被当作处理成功而丢弃——静默丢数据。
//
// 2026-08-17 两处修正：
//
//  1. 原实现在文件打不开/解析失败时直接 `return`，静默当作通过。锚点消失必须表现为
//     红灯，这是 ADR-0091 那四种门控失真形态里的第一种（豁免自己）。改为 exit 2。
//  2. 原实现要求阈值是**字面量 3**（`bin.Y` 必须是 BasicLit "3"）。把 3 提成命名常量
//     是一次纯粹的良性重构，却会让本规则从此一无所获且继续打印 PASS。判据改为
//     「CrashRecoveryCount 与任何东西做上界比较」，不再绑死右操作数的形态。
func checkOutboxWorker(r *lintutil.Reporter) {
	anchorsBefore := r.AnchorCount()

	lintutil.Walk(r, lintutil.WalkOptions{Roots: []string{outboxPath}}, func(f lintutil.File) {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			ifStmt, ok := n.(*ast.IfStmt)
			if !ok || !isCrashCountBound(ifStmt.Cond) {
				return true
			}
			r.Anchor()
			ast.Inspect(ifStmt.Body, func(bn ast.Node) bool {
				ret, ok := bn.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, res := range ret.Results {
					if id, ok := res.(*ast.Ident); ok && id.Name == "nil" {
						r.Violation(f.At(ret), "CrashRecoveryCount 超阈的毒丸分支 return nil，"+
							"消息会被当作处理成功而丢弃（违反 outbox_inv_03 / L-06）")
					}
				}
				return true
			})
			return true
		})
	})

	if r.AnchorCount() == anchorsBefore {
		r.Fatalf("在 %s 里找不到任何 CrashRecoveryCount 上界判定。L-06 的判据锚在这个比较上，"+
			"字段改名或毒丸逻辑被移走都会走到这里；请同步本规则而非让它继续静默通过", outboxPath)
	}
}

// isCrashCountBound 判定条件是否为 CrashRecoveryCount 的上界比较（>= 或 >）。
// 右操作数是字面量还是命名常量都算——绑死字面量会让常量化重构静默废掉这条规则。
func isCrashCountBound(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || (bin.Op != token.GEQ && bin.Op != token.GTR) {
			return true
		}
		if strings.Contains(lintutil.ExprText(bin.X), "CrashRecovery") {
			found = true
		}
		return !found
	})
	return found
}
