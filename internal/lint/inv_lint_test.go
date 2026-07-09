// Package lint_test 静态扫描不变量 CI 测试。
// 使用 go/ast 精确检测调用点，字符串字面量不触发误报。
package lint_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot 返回仓库根目录（此文件在 internal/lint/，向上两级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	// 防卫性校验：存在 go.mod 才是真正的根目录
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		t.Fatalf("repoRoot: go.mod not found under %s", abs)
	}
	return abs
}

// loadExemptFile 从 testdata/ 下的 JSON 文件加载豁免集合（map[string]bool）。
// JSON 格式：{"path/or/key": true, ...}。键名以 "_" 开头的条目为注释，自动跳过。
// convertPath=true 时将键中的斜杠转换为本地路径分隔符（兼容 Windows）；
// convertPath=false 时保留原始键（用于 "relpath:line" 形式的键）。
func loadExemptFile(t *testing.T, root, name string) map[string]bool {
	t.Helper()
	return loadExemptFileOpts(t, root, name, true)
}

// loadExemptFileRaw 与 loadExemptFile 相同，但不转换路径分隔符。
// 适用于键格式为 "relpath:line" 的审计白名单（如 taint_content_approved_calls.json）。
func loadExemptFileRaw(t *testing.T, root, name string) map[string]bool {
	t.Helper()
	return loadExemptFileOpts(t, root, name, false)
}

func loadExemptFileOpts(t *testing.T, root, name string, convertPath bool) map[string]bool {
	t.Helper()
	p := filepath.Join(root, "internal", "lint", "testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("loadExemptFile %s: %v", name, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("loadExemptFile %s: parse error: %v", name, err)
	}
	out := make(map[string]bool, len(raw))
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue // 跳过 _comment 等注释键
		}
		var ok bool
		if err := json.Unmarshal(v, &ok); err != nil || !ok {
			continue
		}
		if convertPath {
			k = filepath.FromSlash(k)
		}
		out[k] = true
	}
	return out
}

// violation 描述一处违规调用点。
type violation struct {
	relPath string
	line    int
	detail  string
}

func (v violation) String() string {
	return fmt.Sprintf("%s:%d: %s", v.relPath, v.line, v.detail)
}

// walkPkgGoFiles 遍历 root/pkg/ 下所有非测试 .go 文件，返回解析后的 AST。
// 跳过 exemptRel 中列出的相对路径（相对于 root）。
func walkPkgGoFiles(t *testing.T, root string, exemptRel map[string]bool,
	fn func(fset *token.FileSet, f *ast.File, relPath string)) {
	t.Helper()
	walkGoFilesUnder(t, root, "pkg", exemptRel, fn)
}

// walkGoFilesUnder 遍历 root/<subdir>/ 下所有非测试 .go 文件，返回解析后的 AST。
// 跳过 exemptRel 中列出的相对路径（相对于 root）。subdir 为空字符串等价于遍历 root 本身。
// 抽出本函数是为了让 pkg/ 专属的检查规则（本文件历史上大量规则只覆盖 pkg/）能够
// 平移到覆盖 internal/ ——2026-07-07 复核发现多条规则因仓库从 pkg/* 迁移到
// internal/* 四层布局后从未同步，长期对着不存在的目录扫描、始终"通过"但没有
// 真正检查任何东西（见 local_playground/reports/code-quality-remediation-verification-20260707.md）。
func walkGoFilesUnder(t *testing.T, root, subdir string, exemptRel map[string]bool,
	fn func(fset *token.FileSet, f *ast.File, relPath string)) {

	t.Helper()
	pkgDir := root
	if subdir != "" {
		pkgDir = filepath.Join(root, subdir)
	}
	err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// .pb.go 是 protoc 生成代码（内部持有大量包级 file_*_proto_* 反射元数据变量，
		// 均由 protoc-gen-go 固定模式生成，不是手写的"裸全局可变状态"），跳过。
		if info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
			return nil //nolint:nilerr
		}
		rel, _ := filepath.Rel(root, path)
		if exemptRel[rel] {
			return nil //nolint:nilerr
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fset := token.NewFileSet()
		// parser.ParseComments：保留 //go:embed 等指令注释挂载到 Doc 字段，
		// 供 Test_inv_NoGlobalVar 识别编译期嵌入常量（2026-07-07 新增需求）。
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return nil //nolint:nilerr // 跳过解析失败的文件（生成代码等）
		}
		fn(fset, f, rel)
		return nil //nolint:nilerr
	})
	if err != nil {
		t.Fatalf("walkPkgGoFiles: %v", err)
	}
}

// isExemptVarInit 判断包级 var 的初始化表达式是否属于豁免类别。
// 豁免类别（不可变或并发安全后置初始化，不是"裸全局可变状态"）：
//  1. errors.New / fmt.Errorf / perrors.New — 哨兵错误（原规则保留）
//  2. sync.OnceValue(...) — 惰性只读单次计算
//  3. regexp.MustCompile(...) — 预编译不可变正则
//  4. []*regexp.Regexp{regexp.MustCompile(...), ...} — 不可变正则表（所有元素均为 MustCompile）
//
// 注意：atomic.Int64/Int32/Pointer 零值变量不在此处豁免——它们是并发安全的可变状态，
// 全局 atomic 变量在可观测性包中通过 ADR-0001 的架构决策维护，由 baseline JSON 豁免。
func isExemptVarInit(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if ok {
		sel, ok2 := call.Fun.(*ast.SelectorExpr)
		if ok2 {
			pkg, ok3 := sel.X.(*ast.Ident)
			if ok3 {
				name := pkg.Name + "." + sel.Sel.Name
				switch name {
				// "perrors.New" 是 pkg/apperr 重命名前的旧别名（历史遗留），实际
				// import path 早已是 "github.com/polarisagi/polaris/pkg/apperr"、
				// 调用点写的是 apperr.New/apperr.Wrap —— 只保留旧名会让本豁免规则
				// 对所有现存 apperr 哨兵错误失效（2026-07-07 复核发现，见
				// local_playground/reports/code-quality-remediation-verification-20260707.md）。
				case "errors.New", "fmt.Errorf", "perrors.New", "apperr.New", "apperr.Wrap",
					"sync.OnceValue",
					"regexp.MustCompile":
					return true
				}
			}
		}
		return false
	}
	// []*regexp.Regexp{regexp.MustCompile(...), ...} 正则表
	lit, ok := expr.(*ast.CompositeLit)
	if ok && len(lit.Elts) > 0 {
		for _, elt := range lit.Elts {
			if !isExemptVarInit(elt) {
				return false
			}
		}
		return true
	}
	return false
}

// isExemptZeroValueType 判断无初始化表达式的包级 var 类型是否属于豁免范畴。
// 适用于 `var x atomic.Int64`、`var x sync.Once` 等零值即可用的并发原语。
// 豁免原则：这些类型的零值语义是正确的、并发安全的，不属于"裸全局可变状态"范畴。
//
// 不豁免（需要保留在 baseline 或彻底重构）：
//   - nil interface / nil 指针（如 metric.Int64Counter、http.Handler）— 可变，需要 InitXxx 赋值
//   - 普通 struct 初始化（如 NewSurpriseIndex()）— 取决于使用方式
func isExemptZeroValueType(typeExpr ast.Expr) bool {
	switch t := typeExpr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		// sync.Once/Mutex/RWMutex/Map 零值即可用，内部自带并发保护，
		// 与 atomic.* 属同一类"零值安全并发原语"（2026-07-07 扩容 internal/ 扫描
		// 范围后发现 internal/gateway/server/chat/sse.go skillEmbedCacheMu、
		// internal/downloader/http.go dlLocks 均属此类，原规则只认
		// sync.Once 是历史遗漏）。
		if pkg.Name == "sync" {
			switch t.Sel.Name {
			case "Once", "Mutex", "RWMutex", "Map":
				return true
			}
		}
		// atomic.Int32/Int64/Uint32/Uint64/Bool/Value 零值合法，并发安全
		if pkg.Name == "atomic" {
			switch t.Sel.Name {
			case "Int32", "Int64", "Uint32", "Uint64", "Bool", "Value", "Pointer":
				return true
			}
		}
		// embed.FS 由 //go:embed 编译器指令填充，编译后不可变，等价于只读常量
		if pkg.Name == "embed" && t.Sel.Name == "FS" {
			return true
		}
	case *ast.IndexExpr:
		// atomic.Pointer[T]（Go 1.18+ 泛型实例化）—— 零值 == nil，Load() 安全返回 nil
		sel, ok := t.X.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		return pkg.Name == "atomic" && sel.Sel.Name == "Pointer"
	}
	return false
}

// ─── inv_M1_01 ────────────────────────────────────────────────────────────────

// Test_inv_M1_01_NoRawHTTPCalls 验证 pkg/ 中不存在裸 HTTP 调用。
// inv_M1_01: 所有 LLM 调用经 Provider Router，禁止裸 http.Get/Post/Head 调用和
//
//	直接引用 http.DefaultClient。
//
// 被扫描的禁止模式：
//   - http.Get(...)  / http.Post(...)  / http.Head(...) — 包级 HTTP 便捷函数
//   - http.DefaultClient — 全局客户端直接引用（绕过 SafeDialer SSRF 防护）
func Test_inv_M1_01_NoRawHTTPCalls(t *testing.T) {
	root := repoRoot(t)
	// 豁免列表由 testdata/raw_http_calls_exempt.json 管理，见该文件注释说明。
	exempt := loadExemptFile(t, root, "raw_http_calls_exempt.json")

	// 禁止的 http 包成员名（调用或引用均算违规）
	forbiddenHTTPSelectors := map[string]bool{
		"Get":           true,
		"Post":          true,
		"Head":          true,
		"DefaultClient": true,
	}

	var violations []violation
	walkPkgGoFiles(t, root, exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkgIdent.Name != "http" {
				return true
			}
			if forbiddenHTTPSelectors[sel.Sel.Name] {
				pos := fset.Position(sel.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  fmt.Sprintf("http.%s — 裸 HTTP 调用/引用，须改用 substrate.NewSafeHTTPClient", sel.Sel.Name),
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_M1_01 VIOLATED: %s", v)
	}
}

// ─── inv_M11_05 / inv_M7_06 ──────────────────────────────────────────────────

// Test_inv_M11_05_NoRawNetDial 验证 pkg/ 中无裸 net.Dial / net.DialContext 调用。
// inv_M11_05: 所有出站连接经 SafeDialer.DialContext 五阶段 SSRF 防护——HTTP/3 QUIC 禁用。
// inv_M7_06:  所有出站连接强制经 M11 SafeDialer.DialContext——禁止裸 net.Dial/grpc.Dial。
//
// 扫描范围: pkg/ 下所有非测试 .go 文件中的 CallExpr（字符串字面量不触发）。
// 精确匹配规则: CallExpr{Fun: SelectorExpr{X: Ident("net"), Sel: "Dial"/"DialContext"}}
//
//	或          CallExpr{Fun: SelectorExpr{X: Ident("grpc"), Sel: "Dial"/"NewClient"}}
func Test_inv_M11_05_NoRawNetDial(t *testing.T) {
	root := repoRoot(t)
	// 豁免列表由 testdata/raw_net_dial_exempt.json 管理，见该文件注释说明。
	exempt := loadExemptFile(t, root, "raw_net_dial_exempt.json")

	// pkg="net", sel in {"Dial","DialContext"} 或 pkg="grpc", sel in {"Dial","NewClient"}
	type forbidden struct{ pkg, sel string }
	forbiddenDialCalls := []forbidden{
		{"net", "Dial"},
		{"net", "DialContext"},
		{"grpc", "Dial"},
		{"grpc", "NewClient"},
	}

	var violations []violation
	walkPkgGoFiles(t, root, exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			// 仅检查 CallExpr 中的 Fun，避免变量名 net.Dialer 之类误报
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, fb := range forbiddenDialCalls {
				if pkgIdent.Name == fb.pkg && sel.Sel.Name == fb.sel {
					pos := fset.Position(call.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  fmt.Sprintf("%s.%s(...) — 裸网络拨号，须改用 substrate.SafeDialer.DialContext", fb.pkg, fb.sel),
					})
				}
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_M11_05/inv_M7_06 VIOLATED: %s", v)
	}
}

// ─── 辅助测试：验证扫描逻辑本身的正确性 ─────────────────────────────────────

// Test_inv_LintScanner_DoesNotFlagStringLiterals 验证扫描器对字符串字面量中的
// "net.Dial" "http.Get" 等模式不产生误报。
// 背景：pkg/cognition/skill_pipeline.go 的 StaticAnalyzer.Analyze 将这些字符串作为
// 被扫描目标（字符串比较），不是实际调用——AST 方案应正确区分。
func Test_inv_LintScanner_DoesNotFlagStringLiterals(t *testing.T) {
	// 构造一个包含字符串字面量"net.Dial"的合成文件，解析后确认无 CallExpr 违规
	src := `package test

import "strings"

func check(code string) bool {
	return strings.Contains(code, "net.Dial") ||
		strings.Contains(code, "http.Get") ||
		strings.Contains(code, "grpc.Dial")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}

	callCount := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name == "net" && (sel.Sel.Name == "Dial" || sel.Sel.Name == "DialContext") {
			callCount++
			t.Errorf("false positive: net.Dial string literal flagged as call at line %d", fset.Position(call.Pos()).Line)
		}
		if pkg.Name == "http" && (sel.Sel.Name == "Get" || sel.Sel.Name == "DefaultClient") {
			callCount++
			t.Errorf("false positive: http.Get string literal flagged as call at line %d", fset.Position(call.Pos()).Line)
		}
		return true
	})

	if callCount > 0 {
		t.Errorf("scanner incorrectly flagged %d string literal(s) as call violations", callCount)
	}
}

// ─── inv_NoCrossLayerImport ──────────────────────────────────────────────────

// Test_inv_NoCrossLayerImport 验证高层包不导入低层反向依赖。
func Test_inv_NoCrossLayerImport(t *testing.T) {
	root := repoRoot(t)

	type layerRule struct {
		layerPrefix             string
		forbiddenImportPrefixes []string
	}

	// 2026-07-07 修正：原规则表键入 pkg/substrate|cognition|action|extensions|swarm/，
	// 这些目录在仓库重构（pkg/* → internal/* 四层布局）后已不存在，导致本检查对着
	// 一批空目录扫描、filepath.Walk 直接因 IsNotExist 提前返回、规则从未被真正触发过
	// （见 local_playground/reports/code-quality-remediation-verification-20260707.md）。
	// 现按 CLAUDE.md 当前四层真实布局重写：
	//   L0: store/observability/security/llm/ffi/protocol/config
	//   L1: agent/action/memory/tool/sandbox/prompt/vfs
	//   L2: swarm/learning/knowledge/extension
	//   L3: gateway/automation/eval/channel/sysmgr/cli
	// 依赖方向必须单向 L0←L1←L2←L3（R1.7），即低层禁止反向 import 高层。
	l1 := []string{"agent", "action", "memory", "tool", "sandbox", "prompt", "vfs"}
	l2 := []string{"swarm", "learning", "knowledge", "extension"}
	l3 := []string{"gateway", "automation", "eval", "channel", "sysmgr", "cli"}

	forbiddenFor := func(pkgs ...string) []string {
		out := make([]string, 0, len(pkgs))
		for _, p := range pkgs {
			out = append(out, "github.com/polarisagi/polaris/internal/"+p+"/")
		}
		return out
	}

	// 2026-07-07 复核发现 internal/sysmgr/{sysinfo,downloader} 被 L0(llm/ollamamgr、
	// llm/stt、llm/tts 下载模型二进制)/L1(agent 的硬件分级、tool/builtin/sys_probe)/
	// L2(extension/marketplace 下载插件包) 广泛引用，但 sysmgr/ 整体按 CLAUDE.md
	// 目录表归类在 L3——这两个子包实质是"系统探测/文件下载"通用能力，不含 L3 特有的
	// 接口治理语义，是被物理放错目录的 L0 工具。经人工确认后物理迁移为
	// internal/sysinfo/ 与 internal/downloader/ 两个独立 L0 顶层包（不再挂靠
	// sysmgr/），无需再对其做跨层豁免——直接并入 l0 列表参与正常规则生成。
	l0 := []string{"store", "observability", "security", "llm", "ffi", "protocol", "config", "sysinfo", "downloader"}
	rules := make([]layerRule, 0, len(l0)+len(l1)+len(l2))
	for _, p := range l0 {
		rules = append(rules, layerRule{
			layerPrefix:             "internal/" + p + "/",
			forbiddenImportPrefixes: append(append(forbiddenFor(l1...), forbiddenFor(l2...)...), forbiddenFor(l3...)...),
		})
	}
	for _, p := range l1 {
		rules = append(rules, layerRule{
			layerPrefix:             "internal/" + p + "/",
			forbiddenImportPrefixes: append(forbiddenFor(l2...), forbiddenFor(l3...)...),
		})
	}
	for _, p := range l2 {
		rules = append(rules, layerRule{
			layerPrefix:             "internal/" + p + "/",
			forbiddenImportPrefixes: forbiddenFor(l3...),
		})
	}

	var violations []violation
	walkGoFilesUnder(t, root, "internal", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		var applicableRule *layerRule
		for _, r := range rules {
			forwardSlashPath := filepath.ToSlash(relPath)
			if strings.HasPrefix(forwardSlashPath, r.layerPrefix) {
				applicableRule = &r
				break
			}
		}

		if applicableRule == nil {
			return
		}

		for _, imp := range f.Imports {
			if imp.Path != nil {
				importPath := strings.Trim(imp.Path.Value, `"`)
				for _, forbiddenPrefix := range applicableRule.forbiddenImportPrefixes {
					if strings.HasPrefix(importPath, forbiddenPrefix) {
						pos := fset.Position(imp.Pos())
						violations = append(violations, violation{
							relPath: relPath,
							line:    pos.Line,
							detail:  fmt.Sprintf("import %q — L0/L1/L2 禁止反向或跨级依赖", importPath),
						})
					}
				}
			}
		}
	})

	for _, v := range violations {
		t.Errorf("inv_NoCrossLayerImport VIOLATED: %s", v)
	}
}

// ─── inv_NoOsExecInHigherLayers ──────────────────────────────────────────────

// Test_inv_NoOsExecInHigherLayers 验证高层禁止直接 os/exec。
func Test_inv_NoOsExecInHigherLayers(t *testing.T) {
	root := repoRoot(t)

	// 2026-07-07 修正：原列表指向 pkg/cognition|swarm|governance|edge|gateway，
	// 这些目录在仓库重构后已不存在（同 Test_inv_NoCrossLayerImport 的修正说明），
	// 导致本检查长期对空目录扫描、从未真正拦截过任何文件。改为 R1.13
	// （docs/specs/00-Constitution.md）明确列出的四个禁止直接 os/exec 的包：
	// internal/agent/ internal/swarm/ internal/eval/ internal/gateway/
	// （install hook 是唯一例外，须经 ContainerSandbox.RunScript，不在此列表放行）。
	highLayerDirs := []string{
		filepath.Join(root, "internal", "agent"),
		filepath.Join(root, "internal", "swarm"),
		filepath.Join(root, "internal", "eval"),
		filepath.Join(root, "internal", "gateway"),
	}

	var violations []violation

	for _, dir := range highLayerDirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil //nolint:nilerr
				}
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr
			}

			relPath, _ := filepath.Rel(root, path)

			src, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
			if err != nil {
				return nil //nolint:nilerr
			}

			for _, imp := range f.Imports {
				if imp.Path != nil && imp.Path.Value == `"os/exec"` {
					pos := fset.Position(imp.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  `import "os/exec" — R1.13 禁止直接 exec，须委托 protocol.ToolRegistry.ExecuteTool + internal/sandbox`,
					})
				}
			}
			return nil //nolint:nilerr
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_NoOsExecInHigherLayers VIOLATED: %s", v)
	}
}

// ─── inv_NoGlobalVarInPkg ────────────────────────────────────────────────────

// Test_inv_NoGlobalVar 验证 pkg/ 与 internal/ 中禁止全局可变变量（R1.3）。
//
// 2026-07-07 修正：原名 Test_inv_NoGlobalVarInPkg，且实现只扫描 pkg/（apperr/types/
// version，三者均无业务逻辑，天然不太会出现可变单例）。R1.3 的真实高发区是
// internal/ 下的 service/policy/classifier 一类包（例如本次复核实际发现并修复的
// internal/security/policy/gate.go EvalTimeout、internal/security/classifier/
// classifier.go Default，见 local_playground/reports/
// code-quality-remediation-verification-20260707.md）——旧实现完全没有覆盖到，
// 相当于给了这一整类问题一个天然盲区。现扩大扫描范围到 internal/，baseline 文件
// 里同步补全了 internal/ 下真实存在的 FFI/OTel 豁免项（原 baseline 里的
// pkg/substrate/、pkg/action/、pkg/cognition/ 等路径同样是旧目录结构下的残留，
// 对应文件早已不存在，已按现状清理并替换为 internal/ 下真实路径）。
func Test_inv_NoGlobalVar(t *testing.T) {
	root := repoRoot(t)

	// 已清理或通过 ADR-0011 / ADR-0001 正式豁免（R1.3 ratchet）。
	// 见 baseline JSON 中的 _permanent_reason 字段。
	// ✅ 已清理（不再出现在本列表）：
	//   agent.go (errTaintViolation, stateToTriggerMap), hooks.go (errNotFound),
	//   reflection_worker.go (defaultReflectionConfig), swarm.go (DefaultAgentLimits),
	//   killswitch.go (PolarisKillswitchStage), env.go (allowedEnvKeys),
	//   curriculum.go (dangerousCommands)
	baselinePath := filepath.Join(root, "internal", "lint", "testdata", "global_var_baseline.json")
	b, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("读取 baseline 失败: %v", err)
	}
	// 用 json.RawMessage 先解析，再按 loadExemptFileOpts 相同逻辑过滤：
	// 跳过 "_" 开头的注释键（如 _comment、_permanent_reason），只处理值为 true 的条目。
	var rawBaseline map[string]json.RawMessage
	if err := json.Unmarshal(b, &rawBaseline); err != nil {
		t.Fatalf("解析 baseline 失败: %v", err)
	}
	exempt := make(map[string]bool)
	for k, v := range rawBaseline {
		if strings.HasPrefix(k, "_") {
			continue
		}
		var ok bool
		if err := json.Unmarshal(v, &ok); err != nil || !ok {
			continue
		}
		exempt[filepath.FromSlash(k)] = true
	}

	checkGlobalVars := func(fset *token.FileSet, f *ast.File, relPath string) []violation {
		var vs []violation
		for _, decl := range f.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.VAR {
				continue
			}
			// //go:embed 编译期嵌入指令：内容由编译器在构建时一次性写入，运行期只读，
			// 与 embed.FS 同等对待（2026-07-07 扩容扫描范围后发现
			// internal/config/integrity_check.go kernelManifestJSON 属此类，
			// 原规则只认 embed.FS 类型、没识别 //go:embed 指令本身）。
			// 注意：ast.CommentGroup.Text() 会主动剥离 "//go:xxx" 这类编译器指令注释
			// （go/ast 文档明确说明），必须直接扫原始 List，不能用 .Text()。
			hasEmbedDirective := func(cg *ast.CommentGroup) bool {
				if cg == nil {
					return false
				}
				for _, c := range cg.List {
					if strings.Contains(c.Text, "go:embed") {
						return true
					}
				}
				return false
			}
			genDeclHasEmbed := hasEmbedDirective(genDecl.Doc)
			for _, spec := range genDecl.Specs {
				valSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				specHasEmbedDirective := genDeclHasEmbed || hasEmbedDirective(valSpec.Doc)

				for i, name := range valSpec.Names {
					if strings.HasPrefix(name.Name, "Err") || name.Name == "_" {
						continue
					}
					if specHasEmbedDirective {
						continue
					}

					if i < len(valSpec.Values) {
						if isExemptVarInit(valSpec.Values[i]) {
							continue
						}
					} else if valSpec.Type != nil && isExemptZeroValueType(valSpec.Type) {
						// 无初始化表达式 + 类型是 atomic.*/sync.Once — 零值即合法
						continue
					}

					pos := fset.Position(valSpec.Pos())
					vs = append(vs, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  fmt.Sprintf("var %s — 全局可变变量，须改为包内局部初始化或注入依赖", name.Name),
					})
				}
			}
		}
		return vs
	}

	var violations []violation
	walkGoFilesUnder(t, root, "pkg", exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		violations = append(violations, checkGlobalVars(fset, f, relPath)...)
	})
	walkGoFilesUnder(t, root, "internal", exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		violations = append(violations, checkGlobalVars(fset, f, relPath)...)
	})

	for _, v := range violations {
		t.Errorf("inv_NoGlobalVar VIOLATED: %s", v)
	}
}

// ─── inv_NoAlterTableInSchema ────────────────────────────────────────────────

// Test_inv_NoAlterTableInSchema 验证禁止使用 ALTER TABLE 补丁。
func Test_inv_NoAlterTableInSchema(t *testing.T) {
	root := repoRoot(t)
	schemaDir := filepath.Join(root, "internal", "protocol", "schema")

	err := filepath.Walk(schemaDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil //nolint:nilerr
			}
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil //nolint:nilerr
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			upperLine := strings.ToUpper(line)
			if strings.Contains(upperLine, "ALTER TABLE") ||
				strings.Contains(upperLine, "ADD COLUMN") ||
				strings.Contains(upperLine, "DROP COLUMN") ||
				strings.Contains(upperLine, "RENAME COLUMN") {
				relPath, _ := filepath.Rel(root, path)
				t.Errorf("inv_NoAlterTableInSchema VIOLATED: %s:%d:\n  %q — 上线前禁止 ALTER TABLE，直接修改原始建表文件并删库重建", relPath, i+1, strings.TrimSpace(line))
			}
		}
		return nil //nolint:nilerr
	})
	if err != nil {
		t.Fatalf("walk %s: %v", schemaDir, err)
	}
}

// ─── inv_NoFFIOutsideFfiPkg ──────────────────────────────────────────────────

// Test_inv_NoFFIOutsideFfiPkg 验证 purego FFI 仅限受控边界使用。
//
// 豁免原则（L0 受控 FFI 边界）：
// 以下文件均属于 pkg/substrate/（L0）或 pkg/action/（L1）内的性能关键路径，
// 且各自拥有独立的 Rust dylib 绑定目标，不具备合并到 pkg/substrate/ffi/ 的价值。
// 每条豁免均需有明确的 Rust FFI 目标说明，禁止以"历史原因"为由新增豁免。
//   - pkg/substrate/ffi/          → purego 桥的权威实现（定义 allowlist 的原点）
//   - pkg/action/tool/rust_*      → Rust WASM/native sandbox，独立 dylib，L1 合法 exec 封装
//   - pkg/substrate/inference/stt → Sherpa-ONNX STT dylib（音频推理，L0 基础设施）
//   - pkg/substrate/inference/tts → Sherpa-ONNX TTS dylib（音频推理，L0 基础设施）
//   - pkg/substrate/policy/cedar  → Cedar 策略引擎 dylib（L0 安全基础设施）
//   - pkg/substrate/storage/surreal → SurrealDB embedded dylib（L0 存储基础设施）
func Test_inv_NoFFIOutsideFfiPkg(t *testing.T) {
	root := repoRoot(t)
	// 豁免列表由 testdata/ffi_boundary_exempt.json 管理，见该文件注释说明。
	exempt := loadExemptFile(t, root, "ffi_boundary_exempt.json")

	var violations []violation
	walkPkgGoFiles(t, root, exempt, func(fset *token.FileSet, f *ast.File, relPath string) {
		for _, imp := range f.Imports {
			if imp.Path != nil && strings.Trim(imp.Path.Value, `"`) == "github.com/ebitengine/purego" {
				pos := fset.Position(imp.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  `import "github.com/ebitengine/purego" — purego FFI 桥仅限 pkg/substrate/ffi/`,
				})
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkgIdent.Name == "purego" && (sel.Sel.Name == "Dlopen" || sel.Sel.Name == "RegisterLibFunc") {
				pos := fset.Position(call.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  fmt.Sprintf("purego.%s(...) — purego FFI 桥仅限 pkg/substrate/ffi/", sel.Sel.Name),
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_NoFFIOutsideFfiPkg VIOLATED: %s", v)
	}
}

// ─── inv_TaintContentCallAudit ───────────────────────────────────────────────

// Test_inv_TaintContentCallAudit 审计 .Content() 调用。
func Test_inv_TaintContentCallAudit(t *testing.T) {
	root := repoRoot(t)
	// 已审计的 .Content() 调用由 testdata/taint_content_approved_calls.json 管理。
	// 每新增一处须在该文件中登记，否则 CI 失败。键格式："relpath:line"，不转换路径分隔符。
	approvedCalls := loadExemptFileRaw(t, root, "taint_content_approved_calls.json")

	var violations []violation
	walkPkgGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		forwardSlashPath := filepath.ToSlash(relPath)
		if strings.HasPrefix(forwardSlashPath, "pkg/substrate/policy/") {
			return
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Content" {
				pos := fset.Position(sel.Pos())
				key := fmt.Sprintf("%s:%d", relPath, pos.Line)
				if !approvedCalls[key] {
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  ".Content() 调用未登记 — 须在 approvedCalls 中审计确认，或改用 SafeString",
					})
				}
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_TaintContentCallAudit VIOLATED: %s", v)
	}
}

// ─── inv_BareErrorReturnRatchet ──────────────────────────────────────────────

// Test_inv_BareErrorReturnRatchet 检测并防范新增 "return err" 裸返回。
func Test_inv_BareErrorReturnRatchet(t *testing.T) {
	root := repoRoot(t)
	baselinePath := filepath.Join(root, "internal", "lint", "testdata", "bare_error_return_baseline.json")

	b, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("读取 baseline 失败: %v", err)
	}
	var baseline map[string]bool
	if err := json.Unmarshal(b, &baseline); err != nil {
		t.Fatalf("解析 baseline 失败: %v", err)
	}

	var violations []violation
	walkPkgGoFiles(t, root, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		ast.Inspect(f, func(n ast.Node) bool {
			retStmt, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range retStmt.Results {
				if ident, isIdent := res.(*ast.Ident); isIdent && ident.Name == "err" {
					pos := fset.Position(retStmt.Pos())
					key := fmt.Sprintf("%s:%d", relPath, pos.Line)
					if !baseline[key] {
						violations = append(violations, violation{
							relPath: relPath,
							line:    pos.Line,
							detail:  `发现新的裸返回 "return err" — 须使用 fmt.Errorf 或 errors.Wrap 等包装错误上下文`,
						})
					}
				}
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_BareErrorReturnRatchet VIOLATED: %s", v)
	}
}

// ─── inv_MCPSubprocessEnvSanitized ───────────────────────────────────────────

// Test_inv_MCPSubprocessEnvSanitized 禁止任何子进程启动器通过 cmd.Env = os.Environ() 或
// cmd.Env = append(os.Environ(), ...) 继承父进程完整环境（R1.15）。
//
// 覆盖范围：
//   - pkg/extensions/  — MCP 子进程（R1.15 核心场景）
//   - pkg/action/      — hook 脚本、X11 工具、sandbox DryRunMode 等所有子进程启动器
//
// 正确做法：使用域专属白名单函数（sanitizeParentEnv / sanitizeHookEnv /
// sanitizeX11Env / sandboxMinEnv）构造子进程环境，再 append 调用方显式注入的变量。
//
// 检测模式：
//  1. *.Env = os.Environ()
//  2. *.Env = append(os.Environ(), ...)
func Test_inv_MCPSubprocessEnvSanitized(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	// isOsEnvironCall 判断一个 AST 表达式是否是 os.Environ() 调用。
	isOsEnvironCall := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "os" && sel.Sel.Name == "Environ"
	}

	// containsOsEnviron 检测表达式是否包含 os.Environ() — 直接调用或作为 append 的第一参数。
	containsOsEnviron := func(expr ast.Expr) bool {
		if isOsEnvironCall(expr) {
			return true
		}
		// append(os.Environ(), ...) 模式
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "append" {
			return false
		}
		return len(call.Args) > 0 && isOsEnvironCall(call.Args[0])
	}

	// scanDir 对单个目录执行 AST 扫描，检测两种违规赋值模式。
	scanDir := func(dir string) error {
		return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr
			}
			relPath, _ := filepath.Rel(root, path)

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return nil //nolint:nilerr
			}

			ast.Inspect(f, func(n ast.Node) bool {
				assignStmt, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range assignStmt.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Env" {
						continue
					}
					if i < len(assignStmt.Rhs) && containsOsEnviron(assignStmt.Rhs[i]) {
						pos := fset.Position(assignStmt.Pos())
						violations = append(violations, violation{
							relPath: relPath,
							line:    pos.Line,
							detail:  `禁止将 cmd.Env 赋为 os.Environ() 或 append(os.Environ(),...)，须用白名单函数构造子进程环境（R1.15）`,
						})
					}
				}
				return true
			})
			return nil //nolint:nilerr
		})
	}

	for _, dir := range []string{
		filepath.Join(root, "pkg", "extensions"), // MCP 子进程
		filepath.Join(root, "pkg", "action"),     // hook / X11 / sandbox 等所有子进程启动器
	} {
		if err := scanDir(dir); err != nil {
			t.Fatalf("walk %s failed: %v", dir, err)
		}
	}

	for _, v := range violations {
		t.Errorf("inv_MCPSubprocessEnvSanitized VIOLATED: %s", v)
	}
}

// ─── inv_NoRawDBExecWriteInGateway ───────────────────────────────────────────

// Test_inv_NoRawDBExecWriteInGateway 禁止 Gateway 层直写 DB（有白名单例外）。
func Test_inv_NoRawDBExecWriteInGateway(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	// 已清理（全部迁移至 Domain Layer）。
	// 此基线文件已清空（{}），此处保留检查逻辑防退化。
	baselinePath := filepath.Join(root, "internal", "lint", "testdata", "gateway_db_write_baseline.json")
	b, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("读取 baseline 失败: %v", err)
	}
	var exempt map[string]bool
	if err := json.Unmarshal(b, &exempt); err != nil {
		t.Fatalf("解析 baseline 失败: %v", err)
	}

	err = filepath.Walk(filepath.Join(root, "pkg", "gateway"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		relPath, _ := filepath.Rel(root, path)

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Exec" || sel.Sel.Name == "ExecContext" || sel.Sel.Name == "Query" || sel.Sel.Name == "QueryContext" {
				if len(call.Args) > 0 {
					var argNode ast.Node
					if (sel.Sel.Name == "ExecContext" || sel.Sel.Name == "QueryContext") && len(call.Args) > 1 {
						argNode = call.Args[1]
					} else {
						argNode = call.Args[0]
					}

					if basicLit, isLit := argNode.(*ast.BasicLit); isLit && basicLit.Kind == token.STRING {
						qStr := strings.ToUpper(basicLit.Value)
						if strings.Contains(qStr, "INSERT ") || strings.Contains(qStr, "UPDATE ") || strings.Contains(qStr, "DELETE ") {
							pos := fset.Position(call.Pos())
							key := fmt.Sprintf("%s:%d", filepath.ToSlash(relPath), pos.Line)
							if !exempt[key] {
								violations = append(violations, violation{
									relPath: relPath,
									line:    pos.Line,
									detail:  fmt.Sprintf(`Gateway 层禁止直接执行写操作 DB.%s — 须通过 Domain Layer`, sel.Sel.Name),
								})
							}
						}
					}
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}

	for _, v := range violations {
		t.Errorf("inv_NoRawDBExecWriteInGateway VIOLATED: %s", v)
	}
}

// ─── inv_NoBareLogPrint ──────────────────────────────────────────────────────

// Test_inv_NoBareLogPrint 禁止在 cmd/polaris 以外使用 fmt.Print/Printf/Println。
func Test_inv_NoBareLogPrint(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	err := filepath.Walk(filepath.Join(root, "pkg"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		relPath, _ := filepath.Rel(root, path)

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkgIdent, isIdent := sel.X.(*ast.Ident); isIdent && pkgIdent.Name == "fmt" {
				if sel.Sel.Name == "Print" || sel.Sel.Name == "Printf" || sel.Sel.Name == "Println" {
					pos := fset.Position(call.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  fmt.Sprintf(`禁止使用 fmt.%s — 业务层须使用结构化日志 (slog)`, sel.Sel.Name),
					})
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}

	for _, v := range violations {
		t.Errorf("inv_NoBareLogPrint VIOLATED: %s", v)
	}
}

// ─── inv_NoRawSQLDBField ─────────────────────────────────────────────────────

// Test_inv_NoRawSQLDBField 禁止在 storage 层以外的包声明 *sql.DB 结构体字段。
// 非 storage 包须持有 protocol.SQLQuerier 或领域 Repository 接口，保证可测试性与边界隔离。
// 白名单：pkg/substrate/storage/ 下所有 repo_*.go + store.go + 基础设施文件，
//
//	以及 pkg/swarm/orchestrator/sqlite_blackboard.go（CAS 原子操作须直接持有 *sql.DB）。
func Test_inv_NoRawSQLDBField(t *testing.T) {
	root := repoRoot(t)

	// 豁免列表由 testdata/sql_db_field_exempt.json 管理，见该文件注释说明。
	exempt := loadExemptFile(t, root, "sql_db_field_exempt.json")

	var violations []violation

	err := filepath.Walk(filepath.Join(root, "pkg"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr
		}
		relPath, _ := filepath.Rel(root, path)
		if exempt[relPath] {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil //nolint:nilerr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				star, ok := field.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				sel, ok := star.X.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				if pkg.Name == "sql" && sel.Sel.Name == "DB" {
					pos := fset.Position(field.Pos())
					violations = append(violations, violation{
						relPath: relPath,
						line:    pos.Line,
						detail:  `禁止在 storage 层外持有 *sql.DB 字段 — 改用 protocol.SQLQuerier 或 Repository 接口`,
					})
				}
			}
			return true
		})
		return nil //nolint:nilerr
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}

	for _, v := range violations {
		t.Errorf("inv_NoRawSQLDBField VIOLATED: %s", v)
	}
}

// ─── inv_NoDirectSemanticMemoryWriteOutsideBuiltin ───────────────────────────

// Test_inv_NoDirectSemanticMemoryWriteOutsideBuiltin 验证禁止在 tool/builtin/、memory/、knowledge/ 之外
// 直接调用 SemanticMemWriter 的写方法 (UpsertFact / UpsertRelation)，
// 防止绕过 Memory-Write-Tool 的统一入口。
func Test_inv_NoDirectSemanticMemoryWriteOutsideBuiltin(t *testing.T) {
	root := repoRoot(t)

	// 允许调用该接口的目录
	allowedDirs := []string{
		filepath.Join("internal", "tool", "builtin"),
		filepath.Join("internal", "memory"),
		filepath.Join("internal", "knowledge"),
	}

	var violations []violation

	walkGoFilesUnder(t, root, "internal", nil, func(fset *token.FileSet, f *ast.File, relPath string) {
		isAllowed := false
		for _, allowed := range allowedDirs {
			// 将路径规范化，防止跨平台路径分隔符不一致导致的问题
			if strings.HasPrefix(filepath.ToSlash(relPath), filepath.ToSlash(allowed)) {
				isAllowed = true
				break
			}
		}
		if isAllowed {
			return
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			// 匹配调用了 UpsertFact, UpsertFactExclusive, UpsertRelation 的地方
			methodName := sel.Sel.Name
			if methodName == "UpsertFact" || methodName == "UpsertFactExclusive" || methodName == "UpsertRelation" {
				pos := fset.Position(call.Pos())
				violations = append(violations, violation{
					relPath: relPath,
					line:    pos.Line,
					detail:  fmt.Sprintf(`调用了 %s — 领先设计防退化：禁止在 tool/builtin/ 外直接调用语义记忆写接口，需通过 ExecuteTool`, methodName),
				})
			}
			return true
		})
	})

	for _, v := range violations {
		t.Errorf("inv_NoDirectSemanticMemoryWriteOutsideBuiltin VIOLATED: %s", v)
	}
}
