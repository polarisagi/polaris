package lint_test

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/polarisagi/polaris/internal/ffi"
)

// ─── inv_FFIABIConstantsInSync ───────────────────────────────────────────────
//
// Go 侧 ffi.ExpectedABIMajor/Minor 与 Rust 侧 SUBSTRATE_ABI_MAJOR/MINOR 必须
// 逐字一致。此前二者仅靠人工同步 + 运行期校验：
//   - major 不匹配 → dylib 加载时 panic（进程起不来才发现）
//   - minor 不匹配 → 只 slog.Warn（生产上很可能没人看见）
//
// 也就是说 minor 漂移可以一直带到线上而无人察觉，major 漂移则要等到部署时
// 才炸。这类"两份常量必须相等"的约束是 100% 机械可检的，不该靠人自觉——
// 与 make deadcode / make docs-refs 同一思路，在 CI 里拦住。
//
// 本测试只读 Rust 源码文本（正则提取常量），不需要编译 Rust，
// 因此在没有 cargo 工具链的环境里同样能跑。

var (
	rustABIMajorRe = regexp.MustCompile(`(?m)^const SUBSTRATE_ABI_MAJOR:\s*u16\s*=\s*(\d+)\s*;`)
	rustABIMinorRe = regexp.MustCompile(`(?m)^const SUBSTRATE_ABI_MINOR:\s*u16\s*=\s*(\d+)\s*;`)
)

func Test_inv_FFIABIConstantsInSync(t *testing.T) {
	root := repoRoot(t)
	libPath := filepath.Join(root, "rust", "substrate", "src", "lib.rs")

	src, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 读取 %s 失败: %v", libPath, err)
	}

	rustMajor := extractRustABIConst(t, rustABIMajorRe, src, "SUBSTRATE_ABI_MAJOR")
	rustMinor := extractRustABIConst(t, rustABIMinorRe, src, "SUBSTRATE_ABI_MINOR")

	if rustMajor != uint64(ffi.ExpectedABIMajor) {
		t.Errorf("inv_FFIABIConstantsInSync VIOLATED: ABI major 不一致——"+
			"Rust SUBSTRATE_ABI_MAJOR=%d，Go ffi.ExpectedABIMajor=%d。"+
			"major 不匹配会在 dylib 加载时 panic；改动任一侧必须同步另一侧"+
			"（并在 rust/substrate/src/lib.rs 的版本注释中记录变更原因）",
			rustMajor, ffi.ExpectedABIMajor)
	}
	if rustMinor != uint64(ffi.ExpectedABIMinor) {
		t.Errorf("inv_FFIABIConstantsInSync VIOLATED: ABI minor 不一致——"+
			"Rust SUBSTRATE_ABI_MINOR=%d，Go ffi.ExpectedABIMinor=%d。"+
			"minor 不匹配在运行期只 slog.Warn，漂移可以一直带到线上无人察觉，"+
			"因此必须在此拦住",
			rustMinor, ffi.ExpectedABIMinor)
	}
}

func extractRustABIConst(t *testing.T, re *regexp.Regexp, src []byte, name string) uint64 {
	t.Helper()
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 在 lib.rs 中未找到 %s 常量定义——"+
			"若该常量被改名或移动，请同步更新本测试的正则", name)
	}
	v, err := strconv.ParseUint(string(m[1]), 10, 16)
	if err != nil {
		t.Fatalf("inv_FFIABIConstantsInSync: 解析 %s 值 %q 失败: %v", name, m[1], err)
	}
	return v
}

// ffiUintptrPointerTarget 识别 `uintptr(unsafe.Pointer(&x))` / `uintptr(unsafe.Pointer(&x[0]))`
// 这一具体写法，返回被擦除为整数的底层标识符名（如 "x"）。
// 只有这种写法才会丢失 GC 指针可达性（ADR-0094 决策五）；形参类型为 *T 或
// unsafe.Pointer 时 GC 保证调用期存活，不在本判据范围内，避免假阳性。
func ffiUintptrPointerTarget(expr ast.Expr) (string, bool) {
	outer, ok := expr.(*ast.CallExpr)
	if !ok || len(outer.Args) != 1 {
		return "", false
	}
	if ident, ok := outer.Fun.(*ast.Ident); !ok || ident.Name != "uintptr" {
		return "", false
	}
	inner, ok := outer.Args[0].(*ast.CallExpr)
	if !ok || len(inner.Args) != 1 {
		return "", false
	}
	sel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "unsafe" || sel.Sel.Name != "Pointer" {
		return "", false
	}
	unary, ok := inner.Args[0].(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return "", false
	}
	switch x := unary.X.(type) {
	case *ast.Ident:
		return x.Name, true
	case *ast.IndexExpr:
		if base, ok := x.X.(*ast.Ident); ok {
			return base.Name, true
		}
	}
	return "", false
}

// TestFFIKeepAliveBoundary (ADR-0094 决策五) AST 扫描 internal/tool/sandbox/ 与
// internal/ffi/，校验每一处 uintptr(unsafe.Pointer(&x)) 写法的 FFI 实参，在其
// 所在函数体内都能找到对应的 runtime.KeepAlive(x)。
//
// 判据严格限定为"实参写法是否把指针擦成了 uintptr"，而不是"函数形参类型是否
// 是 slice/string"——2026-08-10 复核发现旧版规则按函数形参类型判断，对
// rust_wasmtime_sandbox.go 这种形参类型是 *byte（GC 天然保活，不需要
// KeepAlive）的函数也会强制要求补 KeepAlive，产生假阳性，且给 rust_native_sandbox.go
// 真正需要修的 uintptr(unsafe.Pointer(&x[0])) 写法留了不精确的空子。
func TestFFIKeepAliveBoundary(t *testing.T) {
	root := repoRoot(t)
	var violations []violation

	for _, dir := range []string{"internal/tool/sandbox", "internal/ffi"} {
		walkGoFilesUnder(t, root, dir, nil, func(fset *token.FileSet, f *ast.File, relPath string) {
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}

				required := map[string]token.Pos{}
				kept := map[string]bool{}

				ast.Inspect(fn.Body, func(bn ast.Node) bool {
					if call, ok := bn.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
							if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "runtime" && sel.Sel.Name == "KeepAlive" {
								if len(call.Args) == 1 {
									if ident, ok := call.Args[0].(*ast.Ident); ok {
										kept[ident.Name] = true
									}
								}
							}
						}
						for _, arg := range call.Args {
							if name, ok := ffiUintptrPointerTarget(arg); ok {
								if _, exists := required[name]; !exists {
									required[name] = call.Pos()
								}
							}
						}
					}
					return true
				})

				for name, pos := range required {
					if !kept[name] {
						p := fset.Position(pos)
						violations = append(violations, violation{
							relPath: relPath,
							line:    p.Line,
							detail: "uintptr(unsafe.Pointer(&" + name + "...)) in function " + fn.Name.Name +
								" 缺少对应的 runtime.KeepAlive(" + name + ")",
						})
					}
				}
				return true
			})
		})
	}

	for _, v := range violations {
		t.Errorf("FFIKeepAliveBoundary VIOLATED: %s:%d %s", v.relPath, v.line, v.detail)
	}
}
