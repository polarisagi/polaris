//go:build ignore

// comment_drift 扫描 doc comment 与其下方声明错位的「注释漂移」。
//
// 病灶来源：早期机械拆分大文件时，doc comment 与函数体错位——注释描述的是另一个
// 函数，甚至跨文件轮转。已在 agent、gateway/plugin、cronadmin、extension/mcp、
// execute/orchestrator 逐处人工修复过，但每次都是靠人肉通读发现的，没有门控，
// 未排查的模块里必然还有存量。根 CLAUDE.md §维度G 把它列为「本仓库已知系统性病灶」。
//
// 判别式（两条同时成立才报，缺一不可）：
//  1. doc comment 首个 token 是**合法 Go 标识符**且 ≠ 该声明名；
//  2. 该 token **命中同包内另一个声明名**。
//
// 第 2 条是本工具与朴素实现的唯一区别，也是它能用的原因。2026-08-11 那轮留下的
// local_playground/scratch_ast.go 只实现了第 1 条，7 条命中里 6 条是
// `// GET /api/v1/sessions ...` 这类完全正当的路由注释——误报率 86%。
// 按 ADR-0081 决策一同款判断：误报淹没真实缺陷比漏报更致命，宁可收窄。
// 加上第 2 条后，「首词是个标识符、而且恰好是隔壁函数的名字」才是漂移的物证。
//
// 附带检查：文件末尾紧邻 EOF 的孤儿注释块（下方无任何声明）——这是拆分文件时
// 注释被留在原文件的典型残留形态，同样列在 §维度G 的重点排查项里。
//
// 刻意不做：跨文件匹配（同名方法在不同类型上是 Go 常态，跨文件匹配会引入大量
// 误报）；非导出声明（拆分事故集中在导出符号，扩大范围只稀释信噪比）。
//
// 用法:
//
//	go run tools/comment_drift.go                  # 棘轮：baseline 内的存量降为警告
//	go run tools/comment_drift.go -strict          # 忽略 baseline
//	go run tools/comment_drift.go -update-baseline # 收录当前存量
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const driftBaseline = "scripts/comment-drift-baseline.txt"

var scanRoots = []string{"internal", "cmd", "pkg"}

type finding struct {
	file string
	line int
	kind string // drift | orphan
	msg  string
}

func (f finding) sig() string { return fmt.Sprintf("%s|%s|%s", f.file, f.kind, f.msg) }

func main() {
	strict := flag.Bool("strict", false, "忽略 baseline，全部按错误处理")
	update := flag.Bool("update-baseline", false, "把当前存量写入 baseline")
	flag.Parse()

	var findings []finding
	var parseErrs []string

	for _, root := range scanRoots {
		files, err := collectGoFiles(root)
		if err != nil {
			fail("扫描 %s 失败: %v", root, err)
		}
		// 按目录（≈包）分组：判别式第 2 条只在同包内匹配。
		byDir := map[string][]string{}
		for _, f := range files {
			byDir[filepath.Dir(f)] = append(byDir[filepath.Dir(f)], f)
		}
		for _, dir := range sortedKeys(byDir) {
			fs, errs := analyzePackage(byDir[dir])
			findings = append(findings, fs...)
			parseErrs = append(parseErrs, errs...)
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].sig() < findings[j].sig() })

	if *update {
		if err := writeBaseline(driftBaseline, findings); err != nil {
			fail("写 baseline 失败: %v", err)
		}
		fmt.Printf("comment-drift: 已写入 baseline %d 条 → %s\n", len(findings), driftBaseline)
		return
	}

	base := map[string]bool{}
	if !*strict {
		base = readBaseline(driftBaseline)
	}

	var errs, warns []finding
	for _, f := range findings {
		if base[f.sig()] {
			warns = append(warns, f)
		} else {
			errs = append(errs, f)
		}
	}

	for _, e := range parseErrs {
		fmt.Fprintf(os.Stderr, "comment-drift: 解析跳过 %s\n", e)
	}
	if len(errs) == 0 {
		fmt.Printf("comment-drift ok（存量 %d，新增 0）\n", len(warns))
		return
	}
	fmt.Printf("\ncomment-drift FAIL: %d 条新增\n\n", len(errs))
	for _, f := range errs {
		fmt.Printf("  %s:%d [%s] %s\n", f.file, f.line, f.kind, f.msg)
	}
	fmt.Printf("\n订正注释内容，不要动函数体；确属正当写法的可用 -update-baseline 收录（须逐条带理由）\n")
	os.Exit(1)
}

// analyzePackage 对同一目录下的文件做两遍：先收齐声明名，再逐条判定。
func analyzePackage(files []string) ([]finding, []string) {
	fset := token.NewFileSet()
	type parsed struct {
		path string
		file *ast.File
	}
	var asts []parsed
	var parseErrs []string

	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			// 解析失败如实上报，不静默 return nil——那是 nilerr 的成因，
			// 也会让一整个包悄悄退出扫描范围而无人知晓。
			parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		asts = append(asts, parsed{p, f})
	}

	declNames := map[string]bool{}
	for _, a := range asts {
		for name := range declaredNames(a.file) {
			declNames[name] = true
		}
	}

	var out []finding
	for _, a := range asts {
		out = append(out, checkDrift(fset, a.path, a.file, declNames)...)
		out = append(out, checkOrphan(fset, a.path, a.file, declNames)...)
	}
	return out, parseErrs
}

func declaredNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			names[decl.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names[n.Name] = true
					}
				}
			}
		}
	}
	return names
}

func checkDrift(fset *token.FileSet, path string, f *ast.File, declNames map[string]bool) []finding {
	var out []finding
	report := func(pos token.Pos, name, first string) {
		out = append(out, finding{
			file: path,
			line: fset.Position(pos).Line,
			kind: "drift",
			msg:  fmt.Sprintf("doc comment 首词 %q 是同包内另一个声明名，但它挂在 %q 上", first, name),
		})
	}

	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if decl.Doc == nil || !decl.Name.IsExported() {
				continue
			}
			if first, ok := suspectFirstWord(decl.Doc.Text(), decl.Name.Name, declNames); ok {
				report(decl.Pos(), decl.Name.Name, first)
			}
		case *ast.GenDecl:
			if decl.Doc == nil {
				continue
			}
			for _, spec := range decl.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if first, ok := suspectFirstWord(decl.Doc.Text(), ts.Name.Name, declNames); ok {
					report(decl.Pos(), ts.Name.Name, first)
				}
			}
		}
	}
	return out
}

// suspectFirstWord 实现两条判别式。返回 (首词, 是否可疑)。
func suspectFirstWord(doc, declName string, declNames map[string]bool) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(doc))
	if len(fields) == 0 {
		return "", false
	}
	first := strings.TrimRight(fields[0], ".,:;，。：；")
	if first == "" || first == declName {
		return "", false
	}
	if !isGoIdent(first) {
		return "", false // 判别式 1 不成立：`GET`、中文、`//go:` 之类一律放过
	}
	if !declNames[first] {
		return "", false // 判别式 2 不成立：只是个普通英文词，不是隔壁声明的名字
	}
	return first, true
}

// isGoIdent 要求首字符为字母且整体由字母/数字/下划线构成，且**含大小写混排或下划线**
// ——纯小写单词（"the"、"returns"）与纯大写词（"GET"、"TODO"）不算标识符样式。
func isGoIdent(s string) bool {
	r := []rune(s)
	if !unicode.IsLetter(r[0]) {
		return false
	}
	hasUpper, hasLower := false, false
	for _, c := range r {
		switch {
		case unicode.IsUpper(c):
			hasUpper = true
		case unicode.IsLower(c):
			hasLower = true
		case unicode.IsDigit(c) || c == '_':
		default:
			return false
		}
	}
	return hasUpper && hasLower
}

// checkOrphan 找文件末尾紧邻 EOF、下方无任何声明的**doc comment 形态**注释块。
//
// 判别式与 checkDrift 同源：首词命中同包另一个声明名。这一条是必须的——
// 初版只判「末尾 + ≥3 行」，19 条命中里 16 条是文件末尾的设计追记
// （`// [A-03 Step5 决策修正] …`、`// 2026-07-14（ADR-0062）：… 删除——全仓零生产调用点`），
// 那正是根 CLAUDE.md §文档可修订性「追记不改写」要求保留的东西，报它等于叫人删掉决策记录。
// 加上首词判别式后，只有「长得像某个声明的 doc comment、却没有声明跟在后面」才报——
// 那才是拆分文件时注释被留在原处的物证。
func checkOrphan(fset *token.FileSet, path string, f *ast.File, declNames map[string]bool) []finding {
	if len(f.Decls) == 0 {
		return nil
	}
	lastDeclEnd := f.Decls[len(f.Decls)-1].End()
	var out []finding
	for _, cg := range f.Comments {
		if cg.Pos() <= lastDeclEnd {
			continue
		}
		text := strings.TrimSpace(cg.Text())
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		// R7 拆分导航指针（`A / B / C 见 xxx.go（R7 拆分）。`）是本仓库的既定约定，
		// 不是漂移——它恰恰是拆分后防漂移的手段。但指针本身会腐烂，故不放过，
		// 改为校验它：目标文件须存在，且确实声明了所列符号之一。
		if ptrs := parseSplitPointers(text); len(ptrs) > 0 {
			out = append(out, verifySplitPointers(fset, path, cg.Pos(), ptrs, declNames)...)
			continue
		}
		first := strings.TrimRight(fields[0], ".,:;，。：；")
		if !isGoIdent(first) || !declNames[first] {
			continue
		}
		out = append(out, finding{
			file: path,
			line: fset.Position(cg.Pos()).Line,
			kind: "orphan",
			msg:  fmt.Sprintf("文件末尾孤儿注释块以同包声明名 %q 开头，但其下方无任何声明", first),
		})
	}
	return out
}

type splitPointer struct {
	symbols []string
	target  string
}

// parseSplitPointers 解析 `A / B / C 见 xxx.go（R7 拆分）。` 形态的拆分导航指针。
// 一个注释块可含多条（router.go 末尾就有两条），按句号切分后逐条解析。
func parseSplitPointers(text string) []splitPointer {
	var out []splitPointer
	// 一句里可能串多条指针（`…（A/B）见 x.go；…见 y.go。`），故 `。；;` 都作分句符。
	for _, sentence := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '。' || r == '；' || r == ';'
	}) {
		i := strings.Index(sentence, "见 ")
		if i < 0 {
			continue
		}
		head, tail := sentence[:i], sentence[i+len("见 "):]
		var target string
		for _, tok := range strings.FieldsFunc(tail, func(r rune) bool {
			return r == ' ' || r == '（' || r == '(' || r == '\n' || r == ',' || r == '，' || r == '、'
		}) {
			if strings.HasSuffix(tok, ".go") {
				target = tok
				break
			}
		}
		if target == "" {
			continue
		}
		syms := identsIn(head)
		if len(syms) == 0 {
			continue
		}
		out = append(out, splitPointer{symbols: syms, target: target})
	}
	return out
}

// identsIn 从一段中英混排文本里抽出标识符样式的 token。
func identsIn(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return !(unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r) || r == '_')
	}) {
		if len(tok) >= 3 {
			out = append(out, tok)
		}
	}
	return out
}

// verifySplitPointers 校验指针未腐烂：目标文件存在，且其中确实声明了所列符号之一。
//
// 两层收窄，都是实测校准出来的：
//   - 只在「所列 token 至少有一个是本包的真实声明名」时才校验。指针文本里常写的是
//     业务名词而非符号（`apps/plugins 见 repo_extension_apps.go`、
//     `provider_models/CRUD/model/roles 见 providers_models.go`），那不是符号清单，
//     拿它去比对必然误报。
//   - 命中判定只要求「之一」而非全部——同一条指针里常混有 `错误类型定义` 这类描述。
//
// 误报淹没真实缺陷比漏报更致命（同 ADR-0081 决策一）。
func verifySplitPointers(fset *token.FileSet, path string, pos token.Pos, ptrs []splitPointer, pkgNames map[string]bool) []finding {
	dir := filepath.Dir(path)
	line := fset.Position(pos).Line
	var out []finding
	for _, p := range ptrs {
		isSymbolList := false
		for _, s := range p.symbols {
			if pkgNames[s] {
				isSymbolList = true
				break
			}
		}
		if !isSymbolList {
			continue
		}
		target := filepath.Join(dir, p.target)
		names, err := fileDeclNames(target)
		if err != nil {
			out = append(out, finding{path, line, "pointer",
				fmt.Sprintf("拆分指针指向的文件不可读: %s（%v）", p.target, err)})
			continue
		}
		var missing []string
		hit := false
		for _, s := range p.symbols {
			if !pkgNames[s] {
				continue // 非符号描述，不参与判定
			}
			if names[s] {
				hit = true
			} else {
				missing = append(missing, s)
			}
		}
		if !hit {
			out = append(out, finding{path, line, "pointer",
				fmt.Sprintf("拆分指针已腐烂：%s 中找不到所列符号（%s），这些符号已迁往别处",
					p.target, strings.Join(missing, "/"))})
		}
	}
	return out
}

func fileDeclNames(path string) (map[string]bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return declaredNames(f), nil
}

func collectGoFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "pb", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readBaseline(path string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out[line] = true
		}
	}
	return out
}

func writeBaseline(path string, fs []finding) error {
	var sb strings.Builder
	sb.WriteString("# comment-drift 存量基线（棘轮：只禁增量）\n")
	sb.WriteString("# 生成: go run tools/comment_drift.go -update-baseline\n")
	sb.WriteString("# 收窄纪律同 deadcode 白名单——只许逐条审计后删除，禁止批量填充\n")
	for _, f := range fs {
		sb.WriteString(f.sig() + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "comment-drift: "+format+"\n", a...)
	os.Exit(2)
}
