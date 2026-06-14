package action

import (
	"context"
	"strings"

	"github.com/go-python/gpython/ast"
	"github.com/go-python/gpython/parser"
	"github.com/go-python/gpython/py"
	perrors "github.com/polarisagi/polaris/internal/errors"
	"mvdan.cc/sh/v3/syntax"
)

// ASTChecker (L0) 检查 AST 树中是否存在危险节点。
// nil 表示跳过检查。
type ASTChecker interface {
	CheckPython(code []byte) error
	CheckBash(code []byte) error
}

// LLMPeerReviewer (L2) 检查代码意图。
// nil 表示跳过检查。
type LLMPeerReviewer interface {
	Review(ctx context.Context, code string) (risk string, err error)
}

// DefaultASTChecker 实现 L0 静态检查。
type DefaultASTChecker struct{}

// CheckPython 阻止 os.system, subprocess, eval, exec 等危险调用
func (d *DefaultASTChecker) CheckPython(code []byte) error {
	mod, err := parser.ParseString(string(code), py.ExecMode)
	if err != nil {
		return perrors.Wrap(perrors.CodeInvalidInput, "l0_ast: parse python failed", err)
	}

	var visitErr error
	ast.Walk(mod, func(node ast.Ast) bool {
		if visitErr != nil {
			return false
		}
		if node == nil {
			return true
		}

		switch n := node.(type) {
		case *ast.Call:
			// 检查函数名
			if name, ok := n.Func.(*ast.Name); ok {
				id := string(name.Id)
				if id == "eval" || id == "exec" {
					visitErr = perrors.New(perrors.CodeForbidden, "l0_ast: python dangerous builtin call: "+id)
					return false
				}
			} else if attr, ok := n.Func.(*ast.Attribute); ok {
				attrName := string(attr.Attr)
				if attrName == "system" || attrName == "popen" || attrName == "Popen" {
					// 简单过滤
					if val, ok2 := attr.Value.(*ast.Name); ok2 {
						pkg := string(val.Id)
						if pkg == "os" || pkg == "subprocess" {
							visitErr = perrors.New(perrors.CodeForbidden, "l0_ast: python dangerous library call: "+pkg+"."+attrName)
							return false
						}
					}
				}
			}
		case *ast.Import:
			for _, alias := range n.Names {
				name := string(alias.Name)
				if name == "os" || name == "subprocess" || name == "sys" {
					// 可以标记警告，或严格模式下阻断
					// 目前根据需求仅拦截危险调用，这里可以放行或者可选拦截
				}
			}
		}
		return true
	})
	return visitErr
}

// CheckBash 阻止 exec, eval, rm -rf 等
func (d *DefaultASTChecker) CheckBash(code []byte) error {
	f, err := syntax.NewParser().Parse(strings.NewReader(string(code)), "")
	if err != nil {
		return perrors.Wrap(perrors.CodeInvalidInput, "l0_ast: parse bash failed", err)
	}

	var visitErr error
	syntax.Walk(f, func(node syntax.Node) bool {
		if visitErr != nil {
			return false
		}
		if node == nil {
			return true
		}

		switch n := node.(type) {
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				cmd := n.Args[0].Lit()
				if cmd == "exec" || cmd == "eval" {
					visitErr = perrors.New(perrors.CodeForbidden, "l0_ast: bash dangerous command: "+cmd)
					return false
				}
				if cmd == "rm" {
					for _, arg := range n.Args[1:] {
						if strings.Contains(arg.Lit(), "-r") || strings.Contains(arg.Lit(), "-f") {
							visitErr = perrors.New(perrors.CodeForbidden, "l0_ast: bash dangerous command: rm -rf")
							return false
						}
					}
				}
			}
		}
		return true
	})
	return visitErr
}
