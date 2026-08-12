//go:build ignore

// taint_typed_fields_check 校验关键跨界结构体字段是否保持为 taint.TaintedString 类型（F-3）。
//
// 读取 tools/taint-typed-fields.txt 约定的 字段 -> 类型 约束，
// 利用 AST 检查文件中的真实声明。防止重现 CodeActResult.Output 退化为裸 []byte 导致污点丢失（A-4）。
//
// 使用：
//	go run tools/taint_typed_fields_check.go
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

type FieldRequirement struct {
	FilePath     string
	StructName   string
	FieldName    string
	ExpectedType string
}

var errCount int

func main() {
	listPath := "tools/taint-typed-fields.txt"
	reqs, err := parseList(listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taint_typed_fields_check: parse list %s: %v\n", listPath, err)
		os.Exit(2)
	}

	fset := token.NewFileSet()
	checkedCount := 0

	for _, req := range reqs {
		if checkField(fset, req) {
			checkedCount++
		}
	}

	fmt.Printf("taint_typed_fields_check: scanned %d field requirement(s)\n", checkedCount)
	if errCount > 0 {
		fmt.Fprintf(os.Stderr, "taint_typed_fields_check: FAIL — %d violation(s)\n", errCount)
		os.Exit(1)
	}
	fmt.Println("taint_typed_fields_check: PASS")
}

func parseList(path string) ([]FieldRequirement, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var reqs []FieldRequirement
	scanner := bufio.NewScanner(f)
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// internal/protocol/codeact.go:CodeActResult.Output -> taint.TaintedString
		parts := strings.Split(line, "->")
		if len(parts) != 2 {
			continue
		}
		lhs := strings.TrimSpace(parts[0])
		expectedType := strings.TrimSpace(parts[1])

		colonIdx := strings.Index(lhs, ":")
		if colonIdx < 0 {
			continue
		}
		filePath := lhs[:colonIdx]
		structAndField := lhs[colonIdx+1:]

		dotIdx := strings.Index(structAndField, ".")
		if dotIdx < 0 {
			continue
		}
		structName := structAndField[:dotIdx]
		fieldName := structAndField[dotIdx+1:]

		reqs = append(reqs, FieldRequirement{
			FilePath:     filePath,
			StructName:   structName,
			FieldName:    fieldName,
			ExpectedType: expectedType,
		})
	}
	return reqs, scanner.Err()
}

func checkField(fset *token.FileSet, req FieldRequirement) bool {
	node, err := parser.ParseFile(fset, req.FilePath, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taint_typed_fields_check: parse error in %s: %v\n", req.FilePath, err)
		errCount++
		return false
	}

	foundStruct := false
	foundField := false

	ast.Inspect(node, func(n ast.Node) bool {
		typeSpec, ok := n.(*ast.TypeSpec)
		if !ok || typeSpec.Name.Name != req.StructName {
			return true
		}
		structType, ok := typeSpec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		foundStruct = true

		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				if name.Name == req.FieldName {
					foundField = true
					actualType := exprToString(field.Type)
					if actualType != req.ExpectedType {
						pos := fset.Position(field.Pos())
						fmt.Fprintf(os.Stderr, "%s:%d: 字段 %s.%s 的类型为 %q，期望类型为 %q（违反污点防退化约束 F-3）\n",
							req.FilePath, pos.Line, req.StructName, req.FieldName, actualType, req.ExpectedType)
						errCount++
					}
				}
			}
		}
		return true
	})

	if !foundStruct {
		fmt.Fprintf(os.Stderr, "%s: 结构体 %s 未找到\n", req.FilePath, req.StructName)
		errCount++
		return false
	}
	if !foundField {
		fmt.Fprintf(os.Stderr, "%s: 结构体 %s 中未找到字段 %s\n", req.FilePath, req.StructName, req.FieldName)
		errCount++
		return false
	}
	return true
}

func exprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
	case *ast.Ident:
		return t.Name
	case *ast.ArrayType:
		return "[]" + exprToString(t.Elt)
	}
	return fmt.Sprintf("%T", expr)
}
