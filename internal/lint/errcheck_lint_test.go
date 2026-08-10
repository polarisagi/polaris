package lint_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ─── inv_HE1_NoSilentErrorDiscard ────────────────────────────────────────────
//
// 阶段02（02-error-handling.md §3）新增：分级 errcheck-style lint 门控，检测
// `_ = f(...)` / `_, _ = f(...)` 形式的静默错误吞没（对应 HE-1 可观测优先，
// 见 docs/arch/00-Global-Dictionary.md）。
//
// 设计取舍（与本文件其余规则一致，纯 go/ast 静态扫描，不接入 go/types）：
//   - 只匹配「左值全为 `_`」且「右值至少一个是函数/方法调用」的赋值语句，
//     排除 `_ = someVar // 标记故意未使用` 这种非调用丢弃（不是错误吞没）。
//   - 按被调用方法/函数的裸名匹配 errcheck_exempt.json（无法解析接收者静态
//     类型，见该文件 _comment 说明）。
//   - 目录白名单 errcheck_enforced_dirs.json：未接入目录不扫描，非豁免。
//   - baseline 快照 errcheck_baseline.json：阶段02 收尾时已存在但判定为
//     "超出本阶段命名范围、留待后续批次"的调用点（见
//     local_playground/upgrade/99-new-findings.md 对应记录）予以豁免，
//     防止规则一启用就报出与本阶段无关的历史存量。新增代码不受 baseline
//     保护——这是"棘轮"（ratchet）语义：只挡新增，不追溯旧账。

// silentDiscard 描述一处 "_ = f(...)" / "_, _ = f(...)" 形式的调用发现。
type silentDiscard struct {
	relPath  string
	line     int    // 仅用于报错定位，不参与 baseline 键
	funcName string // 所在函数/方法名（方法为 "Recv.Method"）
	callName string // 被丢弃返回值的方法/函数裸名（无法识别时为空串）
	ordinal  int    // 同一 (file, func, callName) 内的出现序号，从 0 开始
}

// key 是 baseline 棘轮的稳定标识。
//
// **刻意不含行号**：baseline 早期以 "file:line" 为键，导致任何在豁免行**之上**
// 的编辑（哪怕只加一行注释）都会让键失配，把一条既有豁免变成"新增违规"，
// CI 假红。这不是理论问题——2026-08-06 一轮修改先后触发了 8 条这样的假失配，
// 每次都要人工回填行号，属于纯粹的维护摩擦，且会训练维护者"报错了就改
// baseline"的坏习惯，反过来削弱棘轮本身的意义。
//
// 改用 (文件, 所在函数, 被丢弃调用名, 同组序号)：函数内代码上下移动、
// 文件其它部分增删都不影响它；只有真正在同一函数内新增同名调用的丢弃点
// 才会产生新键——那恰恰就是棘轮应该拦住的情况。
func (d silentDiscard) key() string {
	return fmt.Sprintf("%s:%s:%s#%d", d.relPath, d.funcName, d.callName, d.ordinal)
}

func (d silentDiscard) String() string {
	return fmt.Sprintf("%s:%d (%s) 静默丢弃调用返回值（`_ = ...`/`_, _ = ...`），需按 HE-1 分级（L1-L4）处理——callName=%s；baseline 键=%s",
		d.relPath, d.line, d.funcName, d.callName, d.key())
}

// enclosingFuncName 返回 AST 节点所在的顶层函数/方法名。
// 方法返回 "Recv.Method" 形式；找不到（包级 var 初始化等）返回 "<file>"。
func enclosingFuncName(f *ast.File, pos token.Pos) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			return recvTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
	return "<file>"
}

// recvTypeName 提取接收者类型名（剥离指针与泛型实参）。
func recvTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(e.X)
	case *ast.IndexExpr:
		return recvTypeName(e.X)
	case *ast.IndexListExpr:
		return recvTypeName(e.X)
	case *ast.Ident:
		return e.Name
	default:
		return "?"
	}
}

// silentDiscardCallName 返回调用表达式的裸方法/函数名（不含包名/接收者），
// 无法识别时返回空串。
func silentDiscardCallName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	default:
		return ""
	}
}

// collectSilentErrorDiscards 在 root/dir 下（跳过 _test.go/.pb.go）查找所有
// "左值全为 `_`，右值含至少一个调用表达式" 的赋值语句。
func collectSilentErrorDiscards(t *testing.T, root, dir string) []silentDiscard {
	t.Helper()
	var found []silentDiscard
	// ordinals 跨文件累计 (file, func, callName) → 已见次数。
	// 必须在 walk 之外声明：同一文件内的多次命中要连续编号。
	ordinals := make(map[string]int)
	walkGoFilesUnder(t, root, dir, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			allBlank := len(assign.Lhs) > 0
			for _, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != "_" {
					allBlank = false
					break
				}
			}
			if !allBlank {
				return true
			}
			var callName string
			hasCall := false
			for _, rhs := range assign.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok {
					hasCall = true
					callName = silentDiscardCallName(call.Fun)
				}
			}
			if !hasCall {
				return true
			}
			pos := fset.Position(assign.Pos())
			fnName := enclosingFuncName(f, assign.Pos())
			// ordinal 在 (file, func, callName) 分组内递增，使同一函数里多个
			// 同名调用的丢弃点各有稳定且互不冲突的键。
			groupKey := relPath + ":" + fnName + ":" + callName
			ord := ordinals[groupKey]
			ordinals[groupKey]++
			found = append(found, silentDiscard{
				relPath:  relPath,
				line:     pos.Line,
				funcName: fnName,
				callName: callName,
				ordinal:  ord,
			})
			return true
		})
	})
	return found
}

func Test_inv_HE1_NoSilentErrorDiscard(t *testing.T) {
	root := repoRoot(t)

	exemptNames := loadExemptFile(t, root, "errcheck_exempt.json")
	enforcedDirs := loadExemptFile(t, root, "errcheck_enforced_dirs.json")
	baseline := loadExemptFileRaw(t, root, "errcheck_baseline.json")

	dirs := make([]string, 0, len(enforcedDirs))
	for d := range enforcedDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	// baseline 键的文件部分必须真实存在（与 global_var_baseline 同款防护，
	// 2026-08-08）：本轮删除 internal/knowledge/ingester.go 时，它的 4 条
	// baseline 条目留了下来——不报错、也不再对应任何代码，只是让"待偿债务
	// 清单"越来越不可信。棘轮的价值建立在条目数可信之上，幽灵条目直接侵蚀
	// 这一点。
	for key := range baseline {
		rel, _, found := strings.Cut(key, ":")
		if !found {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("errcheck_baseline.json 条目 %q 指向不存在的文件 %q——删除/迁移文件时须同步清理其 baseline 条目", key, rel)
		}
	}

	var violations []silentDiscard
	for _, dir := range dirs {
		for _, d := range collectSilentErrorDiscards(t, root, dir) {
			if exemptNames[d.callName] {
				continue
			}
			if baseline[d.key()] {
				continue // 阶段02 结束时已存在、登记为后续批次处理的存量，棘轮豁免
			}
			violations = append(violations, d)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_HE1_NoSilentErrorDiscard VIOLATED: %s", v)
	}
}

// TestGenerateErrcheckBaseline 是 errcheck_baseline.json 的生成器，默认跳过
// （不参与 `make test`/`make lint`）。仅在人工扩展 errcheck_enforced_dirs.json
// 接入新目录、需要为该目录的历史存量重新生成 baseline 快照时，手动执行：
//
//	POLARIS_GEN_ERRCHECK_BASELINE=1 go test ./internal/lint/ -run TestGenerateErrcheckBaseline -v
//
// 生成后必须人工审查新增的 baseline 条目（每条都应能在 99-new-findings.md
// 找到对应的"超出范围、留待后续批次"记录），禁止盲目跑一遍就提交——baseline
// 是"记录已知存量"而非"批量豁免新写的代码"。
func TestGenerateErrcheckBaseline(t *testing.T) {
	if os.Getenv("POLARIS_GEN_ERRCHECK_BASELINE") != "1" {
		t.Skip("baseline 生成器默认跳过，设置 POLARIS_GEN_ERRCHECK_BASELINE=1 手动运行")
	}
	root := repoRoot(t)
	enforcedDirs := loadExemptFile(t, root, "errcheck_enforced_dirs.json")
	exemptNames := loadExemptFile(t, root, "errcheck_exempt.json")

	dirs := make([]string, 0, len(enforcedDirs))
	for d := range enforcedDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	out := make(map[string]bool)
	for _, dir := range dirs {
		for _, d := range collectSilentErrorDiscards(t, root, dir) {
			if exemptNames[d.callName] {
				continue
			}
			out[d.key()] = true
		}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	p := filepath.Join(root, "internal", "lint", "testdata", "errcheck_baseline.json")
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Logf("wrote %d baseline entries to %s", len(out), p)
}

// ─── ADR-0094 决策四：状态落盘不得静默吞错 ──────────────────────────────────
//
// 反例：internal/memory/graph/edge_weight.go 修复前的 ReinforcePath——DB 写入
// 失败时仅 slog.WarnContext 记日志，函数签名不含 error 返回通道，却依然
// `return currentWeight + reinforceRate`（一个凭空算出的"看起来已成功"的伪值），
// 调用方完全不知道写入其实失败了。
//
// 检测范围严格限定为可判定的语法特征子集（本文件其余规则同样只用 go/ast，
// 不接入 go/types，避免语义分析的复杂度与误报面）：
//   - 函数返回值列表中不含 error（有 error 通道的函数不在本规则管辖——它们
//     已经具备"显式返回 error"这条 ADR 允许的合规路径，即使个别调用点没用好
//     也是另一类问题，不属于本规则要抓的"结构上无法报错"）；
//   - 函数体内出现过 dbWriteMethodNames 启发式列表中的方法调用（缩小范围到
//     "看起来在做持久化写"的函数，避免对无关代码的 `if err != nil` 误报）；
//   - `if err != nil { ... }` 块内同时满足：调用了 slog.Warn*/Error* 记录日志，
//     且 return 语句的返回表达式不是裸标识符（ADR 允许"返回未变更的旧值"，
//     即 `return currentWeight` 这种直接透传参数的写法；`return currentWeight
//     + reinforceRate` 这种由表达式现算出的新值才是本规则要拦的"伪造成功值"）。
//
// 这是"棘轮"性质的新规则（同 inv_HE1_NoSilentErrorDiscard），当前代码库应为
// 零违规（edge_weight.go 已在本轮修复为 `(float64, error)`），新增代码若重蹈
// 覆辙会被直接拦下。

var dbWriteMethodNames = map[string]bool{
	"ExecContext": true, "Exec": true, "Update": true, "Delete": true,
	"Put": true, "Save": true,
}

// funcHasErrorReturn 判断函数签名的返回值列表中是否含 error 类型。
func funcHasErrorReturn(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "error" {
			return true
		}
	}
	return false
}

// funcBodyMentionsDBWrite 粗粒度判断函数体内是否出现过 dbWriteMethodNames 中
// 任一方法名的调用（不追踪接收者静态类型，纯裸名匹配，与本文件其余规则的
// 精度取舍一致）。
func funcBodyMentionsDBWrite(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if dbWriteMethodNames[sel.Sel.Name] {
			found = true
		}
		return true
	})
	return found
}

// errIdentName 从形如 `<ident> != nil` 的条件表达式中提取错误变量名
// （要求 ident 名为 "err" 或以 "err"/"Err" 结尾）；不匹配时返回空串。
func errIdentName(cond ast.Expr) string {
	be, ok := cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return ""
	}
	ident, ok := be.X.(*ast.Ident)
	if !ok {
		return ""
	}
	rhs, ok := be.Y.(*ast.Ident)
	if !ok || rhs.Name != "nil" {
		return ""
	}
	if ident.Name == "err" || strings.HasSuffix(ident.Name, "Err") || strings.HasSuffix(ident.Name, "err") {
		return ident.Name
	}
	return ""
}

// isSlogLogCall 判断调用是否形如 slog.Warn*(...)/slog.Error*(...)。
func isSlogLogCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "slog" {
		return false
	}
	return strings.HasPrefix(sel.Sel.Name, "Warn") || strings.HasPrefix(sel.Sel.Name, "Error")
}

// exprReferencesIdent 判断表达式子树内是否出现过指定名字的标识符引用。用于
// 识别 `return Result{Err: apperr.Wrap(..., budgetErr)}` 这类把真实 error
// 变量包进结构体字段/包装函数调用里向外传播的写法——这不是"伪造成功值"，
// 是这个代码库另一种合规的错误传播惯用法（Result-with-Err-field，与
// `(T, error)` 并存），只是语法上不是裸标识符。
func exprReferencesIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return true
	})
	return found
}

// ifBlockLogsWithoutPropagating 判断 if 块「自身直属语句层」（不深入更内层的
// 嵌套 if/else/block——那些会被外层 ast.Inspect 当作独立的 IfStmt 各自单独
// 评估，避免把日志调用和 return 语句从两个互斥的兄弟分支里错误地凑成一对）
// 内是否同时满足：调用了 slog.Warn*/Error*，且存在一个 return 语句的返回
// 表达式既不是裸标识符（透传旧值），也不在子树内引用 errName（说明该表达式
// 没有把真正的错误值传播出去，是凭空算出的"看起来已成功"的伪值）。
func ifBlockLogsWithoutPropagating(block *ast.BlockStmt, errName string) bool {
	hasLog := false
	hasFabricatedReturn := false
	for _, stmt := range block.List {
		switch x := stmt.(type) {
		case *ast.ExprStmt:
			if call, ok := x.X.(*ast.CallExpr); ok && isSlogLogCall(call) {
				hasLog = true
			}
		case *ast.ReturnStmt:
			for _, res := range x.Results {
				if _, isIdent := res.(*ast.Ident); isIdent {
					continue // 裸标识符透传旧值，合规
				}
				if errName != "" && exprReferencesIdent(res, errName) {
					continue // 表达式内部引用了被检查的 err 变量，视为已传播
				}
				hasFabricatedReturn = true
			}
		}
	}
	return hasLog && hasFabricatedReturn
}

// TestDBWriteFailureNotSilentlySwallowed (ADR-0094 决策四) 见本节顶部说明。
func TestDBWriteFailureNotSilentlySwallowed(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	walkRepoGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			if funcHasErrorReturn(fn) {
				return true // 已具备合规的 error 传播通道，不在本规则管辖范围
			}
			if !funcBodyMentionsDBWrite(fn) {
				return true
			}
			ast.Inspect(fn.Body, func(n2 ast.Node) bool {
				ifStmt, ok := n2.(*ast.IfStmt)
				if !ok {
					return true
				}
				errName := errIdentName(ifStmt.Cond)
				if errName == "" {
					return true
				}
				if ifBlockLogsWithoutPropagating(ifStmt.Body, errName) {
					pos := fset.Position(ifStmt.Pos())
					violations = append(violations, violation{
						relPath: relPath, line: pos.Line,
						detail: fn.Name.Name + " 的 DB 写入失败分支只记日志、无 error 返回通道，却 return 了现算出的伪值（应返回未变更旧值或改签名显式返回 error）",
					})
				}
				return true
			})
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("DBWriteFailureNotSilentlySwallowed VIOLATED: %s", v)
	}
}
