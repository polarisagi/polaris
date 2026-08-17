// Package lintutil 收敛 tools/ 下各门控脚本的公共骨架：扫描根、Go 文件遍历与解析、
// 抑制表加载、违规报告与退出码、锚点自毁断言。
//
// 为什么单独成包（2026-08-17）：
//
// tools/ 下每个门控都是独立的 //go:build ignore 单文件 main，此前各自抄了一份
// filepath.Walk + parser.ParseFile + 基线解析。抄出来的差异不是风格问题，是**判据差异**：
//
//   - 扫描根三种写法（internal / internal+pkg / internal+cmd+pkg），四条规则因此
//     从未看过 cmd/ 与 pkg/（ADR-0089 记录过一次同类失效，2026-08-17 又复发一次）；
//   - 13 个工具在 parser.ParseFile 失败时静默 continue，把"读不懂"当成"检查通过"；
//   - 是否跳过 _test.go 各行其是，chan_send_guard 扫测试文件而其余不扫；
//   - 五种互不兼容的基线格式，同一条规则的两个抑制表甚至分居两个目录。
//
// 这些都属于"每处单看都对、合起来没人说得清门控在看哪里"，正是 ADR-0091 认定
// 产出最高的那类问题。收敛到本包后，扫描面由一处定义，改一次全体生效。
//
// 本包是普通包（无 ignore tag），因此进 go vet / golangci-lint / go test 视野，
// 自身有单元测试——门控的地基不该是全仓唯一没人测的代码。
package lintutil

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// ScanRoots 返回本仓全部 Go 源码根。规则若需收窄，用 WalkOptions.Roots 显式声明并在
// 规则头部写明理由——收窄必须是一个有据的决定，不能是抄漏了一个字符串。
//
// 用函数而非包级变量：ADR-0001 禁全局可变变量，而一个可被任意规则改写的扫描根清单
// 恰恰是最糟的那种全局状态——改它等于静默改掉全部规则的判据面。
func ScanRoots() []string { return []string{"internal", "cmd", "pkg"} }

// WalkOptions 描述一条规则的扫描面。零值等价于「三个根、跳过测试文件、无排除」。
type WalkOptions struct {
	// Roots 为空时用 ScanRoots。给具体目录（如 "internal/store"）即收窄到该子树。
	Roots []string
	// IncludeTests 为 true 时把 _test.go 一并纳入扫描。
	IncludeTests bool
	// ExcludeContains 命中即跳过（对斜杠归一化后的路径做子串匹配）。
	ExcludeContains []string
	// NeedComments 为 true 时保留注释节点（读 //nolint: 豁免的规则需要）。
	// 用布尔而非透传 parser.Mode：调用方只有这一个真实需求，暴露整个 Mode 只会让
	// 每个工具都得 import go/parser，且给出「传错 Mode 导致规则静默少看东西」的空间。
	NeedComments bool
}

func (o WalkOptions) parseMode() parser.Mode {
	if o.NeedComments {
		return parser.ParseComments
	}
	return 0
}

func (o WalkOptions) roots() []string {
	if len(o.Roots) == 0 {
		return ScanRoots()
	}
	return o.Roots
}

func (o WalkOptions) skip(path string) bool {
	if !strings.HasSuffix(path, ".go") {
		return true
	}
	if !o.IncludeTests && strings.HasSuffix(path, "_test.go") {
		return true
	}
	slash := filepath.ToSlash(path)
	for _, ex := range o.ExcludeContains {
		if strings.Contains(slash, ex) {
			return true
		}
	}
	return false
}

// File 是一个已解析的 Go 源文件。Path 用斜杠归一化，可直接与基线条目比对。
type File struct {
	Path string
	AST  *ast.File
	Fset *token.FileSet
}

// Pos 返回节点所在行号。
func (f File) Pos(n ast.Node) token.Position { return f.Fset.Position(n.Pos()) }

// At 返回 "path:line" 形式的位置键，是本仓抑制表的统一锚定格式。
func (f File) At(n ast.Node) string {
	return fmt.Sprintf("%s:%d", f.Path, f.Fset.Position(n.Pos()).Line)
}

// Walk 遍历扫描面内的每个 Go 文件，解析后交给 fn。
//
// 解析失败**不静默跳过**：计入 r 的解析错误并继续。这是与旧实现最重要的一处差异——
// 此前 13 个工具在这里 `continue`，一个语法错误的文件会被每一条规则当成"检查通过"。
// go build 确实会拦住语法错误，但门控自己也不能把"读不懂"报成"没问题"。
func Walk(r *Reporter, opts WalkOptions, fn func(File)) {
	fset := token.NewFileSet()
	for _, root := range opts.roots() {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			r.Fatalf("扫描根 %s 不存在——规则的扫描面已失效，请同步 WalkOptions.Roots", root)
		}
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || opts.skip(path) {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, opts.parseMode())
			if perr != nil {
				r.ParseFailure(path, perr)
				return nil
			}
			r.scanned++
			fn(File{Path: filepath.ToSlash(path), AST: f, Fset: fset})
			return nil
		})
		if err != nil {
			r.Fatalf("遍历 %s 失败: %v", root, err)
		}
	}
}
