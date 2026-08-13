//go:build ignore

// taint_typed_fields_check 校验关键跨界结构体字段是否保持为 taint.TaintedString 类型（F-3）。
//
// 读取 tools/taint-typed-fields.txt 约定的 字段 -> 类型 约束，
// 利用 AST 检查文件中的真实声明。防止重现 CodeActResult.Output 退化为裸 []byte 导致污点丢失（A-4）。
//
// 使用：
//
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

// forbiddenMarker 在名单里作为期望类型出现时，语义反转为「该字段必须不存在」。
const forbiddenMarker = "FORBIDDEN"

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

	checkAssignDenylist(fset)

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
	// ExpectedType == "FORBIDDEN" 表示该字段必须不存在（C-8：调用方自报的污点等级
	// 字段一旦重新引入，就会再次诱导后来者拿它做安全判定）。
	if req.ExpectedType == forbiddenMarker {
		if foundField {
			fmt.Fprintf(os.Stderr, "%s: 结构体 %s 不得含字段 %s——该字段由调用方自报，不可作为安全判定输入（违反 F-3/C-8）\n",
				req.FilePath, req.StructName, req.FieldName)
			errCount++
			return false
		}
		return true
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

func checkAssignDenylist(fset *token.FileSet) {
	baseline := make(map[string]bool)
	baselinePath := "local_playground/reports/taint_typed_fields_check-baseline.md"
	if bf, err := os.Open(baselinePath); err == nil {
		scanner := bufio.NewScanner(bf)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				baseline[line] = true
			}
		}
		bf.Close()
	}

	listPath := "tools/taint-assign-denylist.txt"
	f, err := os.Open(listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taint_typed_fields_check: parse list %s: %v\n", listPath, err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		filePath, funcName, desc := parts[0], parts[1], parts[2]

		node, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			continue
		}

		ast.Inspect(node, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}

			if filePath == "internal/agent/context/memory_context.go" && funcName == "WriteUserData" {
				ast.Inspect(fn.Body, func(bn ast.Node) bool {
					call, ok := bn.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "WriteUserData" {
						ast.Inspect(call, func(cn ast.Node) bool {
							kv, ok := cn.(*ast.KeyValueExpr)
							if !ok {
								return true
							}
							keyIdent, ok := kv.Key.(*ast.Ident)
							if ok && keyIdent.Name == "OriginTaintLevel" {
								if _, isSel := kv.Value.(*ast.SelectorExpr); isSel {
									pos := fset.Position(kv.Pos())
									key := fmt.Sprintf("%s:%d", filePath, pos.Line)
									if !baseline[key] {
										fmt.Printf("%s:%d: %s (违反 L-03)\n", filePath, pos.Line, desc)
										errCount++
									}
								}
							}
							return true
						})
					}
					return true
				})
			}

			// 必须连**接收者类型**一起判定：rag_retrieval.go 里有两个 Search，
			// 只有 KnowledgeBase.Search 持有 req（KnowledgeBaseSearchRequest）并真正做
			// TaintMax 过滤；DefaultHybridRetriever.Search 的入参叫 config，压根没有 req。
			// 只按函数名匹配时，后者永远不可能合法满足断言——2026-08-13 轮的做法是
			// 在它函数体里塞两行 `req := config; _ = req.TaintMax` 把文本凑出来，
			// 门控转绿而过滤逻辑一行没有。判据必须落在真正执行过滤的那个函数上。
			if filePath == "internal/knowledge/rag_retrieval.go" && funcName == "Search" &&
				fn.Name.Name == "Search" && receiverTypeName(fn) == "KnowledgeBase" {
				hasTaintMax := false
				ast.Inspect(fn.Body, func(bn ast.Node) bool {
					sel, ok := bn.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "TaintMax" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "req" {
							hasTaintMax = true
						}
					}
					return true
				})

				if !hasTaintMax {
					pos := fset.Position(fn.Pos())
					key := fmt.Sprintf("%s:%d", filePath, pos.Line)
					if !baseline[key] {
						fmt.Printf("%s:%d: %s (违反 L-03)\n", filePath, pos.Line, desc)
						errCount++
					}
				}
			}

			return true
		})
	}
}

// receiverTypeName 返回方法的接收者类型名（指针接收者去掉 *）；非方法返回 ""。
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
