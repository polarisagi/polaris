//go:build ignore

// llm_call_opts_lint 断言每个 Provider 调用都显式声明用途与思考档位（L-19，ADR-0105 决策五）。
//
// 背景：ADR-0105 审计发现 27 个 Provider 调用点未传 WithPurpose、25 个未传 WithThinkingMode。
// 省略思考档位在 DeepSeek 上等于 effort=high，判官类调用（MaxTokens 仅 64）被推理耗尽后输出为空，
// 付了推理费拿不到结果；省略 purpose 则 llm_calls 记为 unspecified，既无法归因，也让精确响应缓存
// 白名单（ADR-0105 决策六，按 purpose 键控）对不上。两项都没有编译期约束，只能靠门控。
//
// 判据面（AST，单文件，无类型信息——与本仓其余门控一致）：
//
//  1. safecall.Infer / safecall.StreamInfer（按文件内 import 别名识别）；
//  2. 任意 X.Infer(ctx, msgs, ...) / X.StreamInfer(ctx, msgs, ...) 形态的方法调用
//     （protocol.Provider、InferenceRouter 等；不做接收者类型判定，宁可多看不漏看）；
//  3. 方法值 / 函数值引用（fn := provider.Infer、传参 safecall.Infer）——调用发生在别处，
//     选项无从校验，一律视为违规，除非豁免。
//
// 合格条件：实参中同时出现 WithPurpose(非空串) 与 WithThinkingMode。opts... 展开时按同函数内
// 可见的构造追踪：
//
//   - 复合字面量 []types.InferOption{...} 与 append(base, ...) 递归展开；
//   - 标识符追溯到本函数内的全部定义（:= / = / var）：各「根定义」取交集，
//     「非条件块内」的自追加 x = append(x, ...) 取并集——if/for/switch/闭包里的追加不算，
//     因为它可能不执行，把它算作满足就是本规则历史上反复出现的「只认一种形态而永远绿灯」；
//   - 函数参数、跨函数调用结果、字段选择、下标等无法静态判定的来源视为「不可判定」。
//     只要其余可见部分已同时含两项即合格（后加的选项只会覆盖取值，不会移除声明）；
//     否则报「无法静态判定」。
//
// 豁免：在调用行或其上一行写 `//llmopts:exempt <理由>`。理由必填，缺理由本身即违规。
// 只应用于纯转发（opts 由上游已过门控的调用方构造）。
//
// 扫描面排除：_test.go、生成代码（Code generated ... DO NOT EDIT）、internal/llm 内部转发实现
// （适配器、路由、注册表记录包装、safecall、响应缓存装饰）——它们本身就是被门控的调用链的一部分，
// opts 原样透传。排除清单按文件列出而非整目录，以免 internal/llm 下新增的调用点被顺带放过。
package main

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/polarisagi/polaris/tools/lintutil"
)

const (
	exemptMarker   = "//llmopts:exempt"
	optPurpose     = "WithPurpose"
	optThinking    = "WithThinkingMode"
	safecallSuffix = "/llm/safecall"
)

func main() {
	r := lintutil.NewReporter("llm-call-opts-check", nil) // fail-closed：存量为 0

	opts := lintutil.WalkOptions{
		NeedComments: true,
		ExcludeContains: []string{
			"internal/llm/adapter/",
			"internal/llm/safecall/",
			"internal/llm/router",
			"internal/llm/provider_registry.go",
			"internal/llm/usage_recorder.go",
			"internal/llm/response_cache.go",
		},
	}
	lintutil.Walk(r, opts, func(f lintutil.File) {
		if isGenerated(f) {
			return
		}
		checkFile(r, f)
	})

	r.RequireAnchors(30, "判据锚在 safecall.Infer/StreamInfer 与 X.Infer/X.StreamInfer 调用上；"+
		"若 Provider 调用入口改名或扫描根变化，请同步 isInferMethod / safecallAlias")
	r.Done()
}

func isGenerated(f lintutil.File) bool {
	for _, g := range f.AST.Comments {
		if g.Pos() > f.AST.Package {
			break
		}
		for _, c := range g.List {
			if strings.Contains(c.Text, "Code generated") && strings.Contains(c.Text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}

func isInferMethod(name string) bool { return name == "Infer" || name == "StreamInfer" }

// safecallAlias 返回文件内 safecall 包的引用名；未导入返回空串。
func safecallAlias(f lintutil.File) string {
	for _, imp := range f.AST.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasSuffix(path, safecallSuffix) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "safecall"
	}
	return ""
}

// exemptLines 收集豁免行（注释行与其下一行）；缺理由的豁免直接报违规。
func exemptLines(r *lintutil.Reporter, f lintutil.File) map[int]bool {
	out := map[int]bool{}
	for _, g := range f.AST.Comments {
		for _, c := range g.List {
			if !strings.HasPrefix(c.Text, exemptMarker) {
				continue
			}
			line := f.Fset.Position(c.Pos()).Line
			if strings.TrimSpace(strings.TrimPrefix(c.Text, exemptMarker)) == "" {
				r.Violation(f.At(c), "%s 必须写明理由（纯转发时说明 opts 由谁在何处声明 purpose/thinking）", exemptMarker)
				continue
			}
			out[line] = true
			out[line+1] = true
		}
	}
	return out
}

func checkFile(r *lintutil.Reporter, f lintutil.File) {
	alias := safecallAlias(f)
	exempt := exemptLines(r, f)

	// 第一遍：登记所有作为调用目标出现的 Infer 选择器，剩余的选择器即方法值/函数值引用。
	callFuns := map[ast.Expr]bool{}
	lintutil.Calls(f.AST, func(call *ast.CallExpr) { callFuns[call.Fun] = true })

	for _, decl := range f.AST.Decls {
		fd, _ := decl.(*ast.FuncDecl)
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !isInferMethod(sel.Sel.Name) {
					return true
				}
				isSafecall := alias != "" && lintutil.ExprText(sel.X) == alias
				// 方法调用至少带 (ctx, msgs)；少于两个实参的同名方法不是 Provider 调用。
				if !isSafecall && len(x.Args) < 2 {
					return true
				}
				r.Anchor()
				if exempt[f.Pos(x).Line] {
					return true
				}
				reportCall(r, f, fd, x, sel.Sel.Name)
			case *ast.SelectorExpr:
				if !isInferMethod(x.Sel.Name) || callFuns[x] {
					return true
				}
				// 只关心「Infer 作为值被取用」：safecall.Infer / <expr>.Infer 出现在非调用位置。
				// 字段赋值目标（recv.Infer = ...）同样无从校验，一并报。
				r.Anchor()
				if exempt[f.Pos(x).Line] {
					return true
				}
				r.Violation(f.At(x), "%s 被当作值引用（方法值/函数值），调用点不在此处，purpose/thinking 无从校验；"+
					"改为直接调用，或加 %s <理由>", lintutil.ExprText(x), exemptMarker)
			}
			return true
		})
	}
}

// optSet 描述一组 InferOption 的静态可见内容。
type optSet struct {
	purpose      bool
	thinking     bool
	emptyPurpose bool // WithPurpose("")：形式上声明了、实际等于没声明
	unresolved   bool // 含无法静态判定的来源（参数、调用结果等）
}

func (a optSet) union(b optSet) optSet {
	return optSet{a.purpose || b.purpose, a.thinking || b.thinking,
		a.emptyPurpose || b.emptyPurpose, a.unresolved || b.unresolved}
}

func (a optSet) intersect(b optSet) optSet {
	return optSet{a.purpose && b.purpose, a.thinking && b.thinking,
		a.emptyPurpose || b.emptyPurpose, a.unresolved || b.unresolved}
}

func reportCall(r *lintutil.Reporter, f lintutil.File, fd *ast.FuncDecl, call *ast.CallExpr, method string) {
	var got optSet
	if len(call.Args) > 2 {
		rs := &resolver{fd: fd, visiting: map[string]bool{}}
		rest := call.Args[2:]
		for i, a := range rest {
			if call.Ellipsis.IsValid() && i == len(rest)-1 {
				got = got.union(rs.expr(a))
			} else {
				got = got.union(optionOf(a))
			}
		}
	}
	switch {
	case got.emptyPurpose:
		r.Violation(f.At(call), "%s：WithPurpose 传了空串，等同未声明（llm_calls 记为 unspecified）", method)
	case got.purpose && got.thinking:
		// 合格
	case got.unresolved:
		r.Violation(f.At(call), "%s：opts 含无法静态判定的来源（函数参数/调用结果等），且可见部分未同时含 "+
			"WithPurpose 与 WithThinkingMode；在同函数内构造后再传入，或纯转发时加 %s <理由>", method, exemptMarker)
	default:
		var miss []string
		if !got.purpose {
			miss = append(miss, optPurpose)
		}
		if !got.thinking {
			miss = append(miss, optThinking)
		}
		r.Violation(f.At(call), "%s 调用缺少 %s（ADR-0105 决策五）", method, strings.Join(miss, " 与 "))
	}
}

// optionOf 识别单个实参是否为 WithPurpose(...) / WithThinkingMode(...) 调用。
func optionOf(e ast.Expr) optSet {
	call, ok := unparen(e).(*ast.CallExpr)
	if !ok {
		return optSet{}
	}
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	switch name {
	case optPurpose:
		s := optSet{purpose: true}
		if len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING &&
				(lit.Value == `""` || lit.Value == "``") {
				s.emptyPurpose = true
			}
		}
		return s
	case optThinking:
		return optSet{thinking: true}
	}
	return optSet{}
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// resolver 在单个顶层函数内追溯 opts 切片的构造。
type resolver struct {
	fd       *ast.FuncDecl
	visiting map[string]bool
}

// expr 展开一个「切片类」表达式的可见内容。
func (rs *resolver) expr(e ast.Expr) optSet {
	switch x := unparen(e).(type) {
	case *ast.CompositeLit:
		var s optSet
		for _, el := range x.Elts {
			s = s.union(optionOf(el))
		}
		return s
	case *ast.SliceExpr:
		return rs.expr(x.X)
	case *ast.Ident:
		return rs.ident(x.Name)
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "append" && len(x.Args) >= 1 {
			return rs.expr(x.Args[0]).union(rs.appendAdds(x))
		}
	}
	return optSet{unresolved: true}
}

// appendAdds 是 append(base, adds...) 中 base 之外追加的内容。
func (rs *resolver) appendAdds(call *ast.CallExpr) optSet {
	var s optSet
	rest := call.Args[1:]
	for i, a := range rest {
		if call.Ellipsis.IsValid() && i == len(rest)-1 {
			s = s.union(rs.expr(a))
		} else {
			s = s.union(optionOf(a))
		}
	}
	return s
}

type def struct {
	rhs   ast.Expr // nil = 无初值（var x T）或多值赋值无法拆分
	multi bool     // 多值赋值：无法逐项对应
	depth int      // 条件嵌套深度
}

func isConditional(n ast.Node) bool {
	switch n.(type) {
	case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt,
		*ast.SelectStmt, *ast.CaseClause, *ast.CommClause, *ast.FuncLit:
		return true
	}
	return false
}

// defsOf 收集函数内对 name 的全部定义与赋值，并记录各自的条件嵌套深度。
func (rs *resolver) defsOf(name string) (defs []def, isParam bool) {
	if rs.fd == nil || rs.fd.Body == nil {
		return nil, false
	}
	collectParams := func(ft *ast.FuncType) {
		if ft == nil || ft.Params == nil {
			return
		}
		for _, p := range ft.Params.List {
			for _, n := range p.Names {
				if n.Name == name {
					isParam = true
				}
			}
		}
	}
	collectParams(rs.fd.Type)

	var stack []ast.Node
	depth := func() int {
		d := 0
		for _, s := range stack {
			if isConditional(s) {
				d++
			}
		}
		return d
	}
	ast.Inspect(rs.fd.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.FuncLit:
			collectParams(x.Type)
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				d := def{depth: depth()}
				if len(x.Rhs) == len(x.Lhs) {
					d.rhs = x.Rhs[i]
				} else {
					d.multi = true
				}
				defs = append(defs, d)
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name != name {
					continue
				}
				d := def{depth: depth()}
				if i < len(x.Values) {
					d.rhs = x.Values[i]
				}
				defs = append(defs, d)
			}
		}
		return true
	})
	return defs, isParam
}

func isSelfAppend(d def, name string) bool {
	call, ok := d.rhs.(*ast.CallExpr)
	if !ok || d.rhs == nil {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "append" || len(call.Args) < 1 {
		return false
	}
	base := unparen(call.Args[0])
	if s, ok := base.(*ast.SliceExpr); ok {
		base = unparen(s.X)
	}
	b, ok := base.(*ast.Ident)
	return ok && b.Name == name
}

// ident 追溯标识符 name 在本函数内的构造。
func (rs *resolver) ident(name string) optSet {
	if rs.visiting[name] {
		return optSet{unresolved: true}
	}
	rs.visiting[name] = true
	defer delete(rs.visiting, name)

	defs, isParam := rs.defsOf(name)
	var roots, selfs []def
	for _, d := range defs {
		if isSelfAppend(d, name) {
			selfs = append(selfs, d)
		} else {
			roots = append(roots, d)
		}
	}

	var base optSet
	baseDepth := 0
	switch {
	case len(roots) == 0:
		// 只有参数或包级变量：来源不可见。
		base = optSet{unresolved: true}
	default:
		baseDepth = roots[0].depth
		for i, d := range roots {
			var s optSet
			switch {
			case d.multi:
				s = optSet{unresolved: true}
			case d.rhs == nil:
				s = optSet{} // var x []T：空切片，可见且为空
			default:
				s = rs.expr(d.rhs)
			}
			if i == 0 {
				base = s
			} else {
				base = base.intersect(s)
			}
			baseDepth = min(baseDepth, d.depth)
		}
	}
	// 参数与根定义并存（形参被重新赋值）时，形参传入的内容同样不可见。
	if isParam && len(roots) > 0 {
		base.unresolved = true
	}

	for _, d := range selfs {
		// 条件块内的追加可能不执行，不计入「已声明」。
		if d.depth > baseDepth {
			continue
		}
		base = base.union(rs.appendAdds(d.rhs.(*ast.CallExpr)))
	}
	return base
}
