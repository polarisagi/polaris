//go:build ignore

// chan_send_guard_lint 断言向 ResultCh 的发送都包在 select-default 里（L-05）。
//
// 背景：ResultCh 的接收方是调用者，可能在超时后直接走人。此时单写者若做裸发送
// `ch <- v`，会永久阻塞在一个再也没人读的 channel 上——goroutine 泄漏，且泄漏的是
// 持有请求上下文的那一个，内存随请求量线性涨。正确写法是
// `select { case ch <- v: default: }` 或带 ctx.Done() 的多路 select。
//
// 扫描根 2026-08-17 从 internal/store 扩到全仓三根。原注释称收窄是因为「ResultCh 是
// store 包的约定字段名」，实测不成立：internal/learning/surprise/surprise.go 也有一个
// 同语义的 ResultCh 发送，风险一模一样，却从不在本规则视野内。收窄的理由若经不起
// 一次 grep，那它就不是收窄而是抄漏（ADR-0089）。
//
// 豁免：在发送行或其上一行写 //nolint:chan_send_guard 并说明理由。注意 select 的
// `case ch <- v:` 本身就是合规形态，不需要 nolint——同轮清掉了两个挂在合规 case 上的
// 死豁免（mutation_bus_execute.go），它们看起来像"这里有已知问题"，实际什么也没抑制。
package main

import (
	"go/ast"

	"github.com/polarisagi/polaris/tools/lintutil"
)

const guardedChanField = "ResultCh"

func main() {
	r := lintutil.NewReporter("chan-send-guard-lint", nil) // fail-closed：存量为 0

	lintutil.Walk(r, lintutil.WalkOptions{NeedComments: true}, func(f lintutil.File) {
		exempt := lintutil.NolintLines(f, "chan_send_guard")
		guarded, unguarded := classifySends(f.AST)
		// 合规的发送也计入判据面：锚点数要反映"这条规则看得见多少个 ResultCh 发送"，
		// 而不是"报了多少红"。只数违规的话，字段改名会让锚点归零却依旧打印 PASS。
		r.Anchors(guarded)
		for _, send := range unguarded {
			r.Anchor()
			if exempt[f.Fset.Position(send.Pos()).Line] {
				continue
			}
			r.Violation(f.At(send), "%s 发送未包裹在 select 中，接收方超时离开后单写者会永久阻塞"+
				"（违反 L-05）", guardedChanField)
		}
	})

	r.RequireAnchors(1, "判据锚在对 .ResultCh 的发送语句上；字段改名或链路重写时请同步 guardedChanField")
	r.Done()
}

// classifySends 把全文的 ResultCh 发送分成「已被 select case 守护」与「裸发送」两类。
//
// 判定方式：先把所有作为 CommClause.Comm 出现的发送语句登记为已守护，再遍历全文收集
// ResultCh 发送，差集即违规。
//
// 2026-08-17 重写。原实现手工递归 select 的 Body 并 return false 中断默认遍历，中间
// 留着三段自问自答的草稿注释和一个空的 if 分支；更要紧的是它对「select 内嵌 select」
// 与「case 体内的裸发送」的处理靠遍历顺序隐式成立，改一行就可能翻车。集合差集没有这个问题。
func classifySends(f *ast.File) (guardedCount int, unguarded []*ast.SendStmt) {
	guarded := map[*ast.SendStmt]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, clause := range sel.Body.List {
			if cc, ok := clause.(*ast.CommClause); ok {
				if send, ok := cc.Comm.(*ast.SendStmt); ok {
					guarded[send] = true
				}
			}
		}
		return true
	})

	ast.Inspect(f, func(n ast.Node) bool {
		send, ok := n.(*ast.SendStmt)
		if !ok {
			return true
		}
		sel, ok := send.Chan.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != guardedChanField {
			return true
		}
		if guarded[send] {
			guardedCount++
		} else {
			unguarded = append(unguarded, send)
		}
		return true
	})
	return guardedCount, unguarded
}
