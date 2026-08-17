//go:build ignore

// fsm_io_lint 守 internal/agent/fsm 的两条不变量：
//
//	[inv_FSM_B1] Transition.Effects 闭包内禁止持锁同步 IO（F-4）
//	[L-16]       FSM 包内禁止 goto —— 控制流必须由状态机显式承载（HE-5）
//
// ── inv_FSM_B1 ──
// 展开深度：Effects 闭包体 + 它直接调用的同包函数/方法体（一层调用图展开）。
//
//	2026-08-12：此前只扫闭包体本身，对 B-1 的真实形态完全失明——
//	Effects → sm.trySystem1Bypass(sCtx) → sm.skillMatcher.MatchIntent(...)，
//	IO 藏在被调方法里。负向验证（把 MatchIntent 调用塞回 trySystem1Bypass）当时
//	仍报 PASS，等于这条门控从未防住它要防的那个缺陷。
//
// 已知局限：只展开一层。A→B→C 的深层 IO 抓不到，依赖 code review。这是成本/收益的
// 折中——展开全图需要类型信息，而 B-1 这类「闭包直接调一个私有 helper」是实际发生过
// 的形态，一层已覆盖。不要把本工具当作完备性保证。
//
// 外部名单：tools/fsm-io-denylist.txt（**规则输入表**，非抑制表，故留在 tools/ 下）。
//
// ── L-16 ──
// 2026-08-17 从 Makefile 的内联 `grep -rn 'goto '` 目标搬进来并 AST 化。原目标挂在
// GD-14-006 这个**批次内序号**下（review_check.go 自己写明 GD 编号跨轮复用），而
// internal/lint/fsm_lint_test.go 里另有一条完全不同的断言也叫 GD-14-006——一个 ID
// 标着两条互不相干的规则，而 lint_selftest 正是按 ID 数门控条数的。改为稳定的 L-16，
// 并登记负向用例（此前它作为"Makefile 内联 grep"被整体豁免于负向验证）。
// grep 版还有个实际缺陷：字符串字面量里出现 "goto " 也会命中。
package main

import (
	"go/ast"
	"os"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

const (
	fsmDir       = "internal/agent/fsm"
	denylistPath = "tools/fsm-io-denylist.txt"
)

func main() {
	denylist := loadDenylist()
	r := lintutil.NewReporter("fsm-io-lint", nil) // fail-closed：FSM 锁内 IO 不接受存量

	var files []lintutil.File
	lintutil.Walk(r, lintutil.WalkOptions{Roots: []string{fsmDir}}, func(f lintutil.File) {
		files = append(files, f)
	})

	// 先建同包函数/方法索引，供 Effects 闭包做一层调用图展开。
	pkgFuncs := indexPackageFuncs(files)
	for _, f := range files {
		checkEffects(r, f, denylist, pkgFuncs)
		checkNoGoto(r, f)
	}

	r.RequireAnchors(1, "判据锚在 fsm 包的 Transition{...Effects: func...} 字面量上；"+
		"一个闭包都找不到说明状态机的声明形态变了，请同步本规则")
	r.Done()
}

// ─── L-16：FSM 包内禁 goto ────────────────────────────────────────────────────

// checkNoGoto 拦截 fsm 包内的 goto 语句。
//
// HE-5 要求控制流由 Go FSM 显式承载：状态转移应当表现为 Transition 表里的一条边，
// 而不是一个跳到某个标签的 goto——后者让「这个状态能到哪些状态」不再可以从表里读出来，
// 而 FSM 表正是这套架构里唯一能被审计的控制流真相。
func checkNoGoto(r *lintutil.Reporter, f lintutil.File) {
	ast.Inspect(f.AST, func(n ast.Node) bool {
		br, ok := n.(*ast.BranchStmt)
		if !ok || br.Tok.String() != "goto" {
			return true
		}
		label := ""
		if br.Label != nil {
			label = br.Label.Name
		}
		r.Violation(f.At(br), "FSM 包内出现 goto %s——状态转移必须是 Transition 表里的一条边，"+
			"goto 会让控制流从表里读不出来（违反 HE-5 / L-16）", label)
		return true
	})
}

// ─── inv_FSM_B1：Effects 闭包内禁同步 IO ──────────────────────────────────────

// pkgFunc 记录同包函数/方法的函数体与其所在文件，供一层调用图展开使用。
type pkgFunc struct {
	body *ast.BlockStmt
	path string
}

// indexPackageFuncs 按函数名索引同包所有顶层函数与方法（方法只按方法名索引，
// 不区分 receiver 类型——fsm 包内方法名不重复，够用且避免引入类型检查依赖）。
func indexPackageFuncs(files []lintutil.File) map[string]pkgFunc {
	idx := make(map[string]pkgFunc)
	for _, f := range files {
		lintutil.FuncDecls(f, func(fd *ast.FuncDecl) {
			idx[fd.Name.Name] = pkgFunc{body: fd.Body, path: f.Path}
		})
	}
	return idx
}

func checkEffects(r *lintutil.Reporter, f lintutil.File, denylist map[string]bool, pkgFuncs map[string]pkgFunc) {
	ast.Inspect(f.AST, func(n ast.Node) bool {
		composite, ok := n.(*ast.CompositeLit)
		if !ok || !strings.HasSuffix(lintutil.ExprText(composite.Type), "Transition") {
			return true
		}
		for _, elt := range composite.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Effects" {
				continue
			}
			funcLit, ok := kv.Value.(*ast.FuncLit)
			if !ok || funcLit.Body == nil {
				continue
			}
			r.Anchor()
			scanBody(r, f, funcLit.Body, denylist, pkgFuncs, "", true)
		}
		return true
	})
}

// scanBody 扫描一个函数体。expand 为 true 时，对体内调用的同包函数再下探一层。
func scanBody(
	r *lintutil.Reporter, f lintutil.File, body *ast.BlockStmt,
	denylist map[string]bool, pkgFuncs map[string]pkgFunc, via string, expand bool,
) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		full := lintutil.ExprText(call.Fun)
		// SafeGo 内的 IO 是异步非阻塞，不占 FSM 锁，放行且不再下探。
		if strings.Contains(full, "SafeGo") {
			return false
		}

		name := full
		if i := strings.LastIndex(full, "."); i >= 0 {
			name = full[i+1:]
		}
		if name == "" {
			return true
		}

		if denylist[name] {
			if via == "" {
				r.Violation(f.At(call), "Effects 闭包内包含黑名单 IO 方法调用 %q"+
					"（违反 inv_FSM_B1，锁内禁止同步 IO）", name)
			} else {
				r.Violation(f.At(call), "Effects 闭包经 %s() 间接调用黑名单 IO 方法 %q"+
					"（违反 inv_FSM_B1，锁内禁止同步 IO）", via, name)
			}
			return true
		}

		if expand {
			if target, ok := pkgFuncs[name]; ok && target.body != body {
				scanBody(r, lintutil.File{Path: target.path, AST: f.AST, Fset: f.Fset},
					target.body, denylist, pkgFuncs, name, false)
			}
		}
		return true
	})
}

func loadDenylist() map[string]bool {
	data, err := os.ReadFile(denylistPath)
	if err != nil {
		// 名单即判据，读不到就没有判据可言。
		os.Stderr.WriteString("fsm-io-lint: 无法读取 IO 黑名单 " + denylistPath + ": " + err.Error() + "\n")
		os.Exit(2)
	}
	list := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		list[line] = true
	}
	if len(list) == 0 {
		os.Stderr.WriteString("fsm-io-lint: IO 黑名单为空，规则没有判据\n")
		os.Exit(2)
	}
	return list
}
