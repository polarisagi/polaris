package action

import (
	"context"
	"strings"

	"github.com/go-python/gpython/ast"
	"github.com/go-python/gpython/parser"
	"github.com/go-python/gpython/py"

	"mvdan.cc/sh/v3/syntax"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// LLMPeerReviewer (L2) 检查代码意图。
// nil 表示跳过检查。
type LLMPeerReviewer interface {
	Review(ctx context.Context, code string) (risk string, err error)
}

// ASTChecker (L0) 静态语言级检查器
// nil 表示跳过检查。
type ASTChecker interface {
	CheckPython(code []byte) error
	CheckBash(code []byte) error
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
		if visitErr != nil || node == nil {
			return visitErr == nil && node == nil
		}

		switch n := node.(type) {
		case *ast.Call:
			visitErr = d.checkPythonCall(n)
			if visitErr != nil {
				return false
			}
		case *ast.Import:
			// 可以标记警告，或严格模式下阻断
			// 目前根据需求仅拦截危险调用，这里可以放行或者可选拦截
		}
		return true
	})
	return visitErr
}

func (d *DefaultASTChecker) checkPythonCall(n *ast.Call) error {
	if name, ok := n.Func.(*ast.Name); ok {
		id := string(name.Id)
		if id == "eval" || id == "exec" {
			return perrors.New(perrors.CodeForbidden, "l0_ast: python dangerous builtin call: "+id)
		}
		return nil
	}

	attr, ok := n.Func.(*ast.Attribute)
	if !ok {
		return nil
	}

	attrName := string(attr.Attr)
	if attrName != "system" && attrName != "popen" && attrName != "Popen" {
		return nil
	}

	val, ok2 := attr.Value.(*ast.Name)
	if !ok2 {
		return nil
	}

	pkg := string(val.Id)
	if pkg == "os" || pkg == "subprocess" {
		return perrors.New(perrors.CodeForbidden, "l0_ast: python dangerous library call: "+pkg+"."+attrName)
	}

	return nil
}

// CheckBash 阻止 exec, eval, rm -rf 等
func (d *DefaultASTChecker) CheckBash(code []byte) error {
	f, err := syntax.NewParser().Parse(strings.NewReader(string(code)), "")
	if err != nil {
		return perrors.Wrap(perrors.CodeInvalidInput, "l0_ast: parse bash failed", err)
	}

	var visitErr error
	syntax.Walk(f, func(node syntax.Node) bool {
		if visitErr != nil || node == nil {
			return visitErr == nil && node == nil
		}

		cmd, ok := node.(*syntax.CallExpr)
		if !ok || len(cmd.Args) == 0 {
			return true
		}

		name := cmd.Args[0].Parts[0]
		word, ok2 := name.(*syntax.Lit)
		if !ok2 {
			return true
		}

		if word.Value == "eval" || word.Value == "exec" {
			visitErr = perrors.New(perrors.CodeForbidden, "l0_ast: bash dangerous builtin call: "+word.Value)
			return false
		}

		if word.Value == "rm" && len(cmd.Args) > 1 {
			visitErr = d.checkBashRM(cmd)
			if visitErr != nil {
				return false
			}
		}

		return true
	})
	return visitErr
}

func (d *DefaultASTChecker) checkBashRM(cmd *syntax.CallExpr) error {
	for _, arg := range cmd.Args[1:] {
		if len(arg.Parts) > 0 {
			if word, ok := arg.Parts[0].(*syntax.Lit); ok {
				if strings.Contains(word.Value, "-r") || strings.Contains(word.Value, "-f") {
					return perrors.New(perrors.CodeForbidden, "l0_ast: bash dangerous command: rm -rf")
				}
			}
		}
	}
	return nil
}
