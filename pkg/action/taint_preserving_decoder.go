package action

import (
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
)

// nodeKind JSON 节点类型枚举。
type nodeKind uint8

const (
	kindNull nodeKind = iota
	kindString
	kindNumber
	kindBool
	kindArray
	kindObject
)

// TaintedJSONNode MCP JSON 响应的污点标注树节点。
//
// string 叶子节点持有显式 TaintLevel；复合节点的 Taint 字段为
// 所有子节点的最高污点值（在 walk 阶段完成）。
// 非 string 标量（number/bool/null）Taint 保持 TaintNone，仅作路径溯源。
//
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §1 TaintPreservingDecoder
type TaintedJSONNode struct {
	Kind     nodeKind
	StrVal   string
	NumVal   float64
	BoolVal  bool
	ArrNodes []*TaintedJSONNode
	ObjNodes map[string]*TaintedJSONNode
	Taint    protocol.TaintLevel
	JSONPath string
}

// MaxTaint 返回当前节点及所有后代中最高的污点等级。
// 实现 PropagateTaint 语义：只升不降。
func (n *TaintedJSONNode) MaxTaint() protocol.TaintLevel {
	if n == nil {
		return protocol.TaintNone
	}
	max := n.Taint
	for _, c := range n.ArrNodes {
		if t := c.MaxTaint(); t > max {
			max = t
		}
	}
	for _, c := range n.ObjNodes {
		if t := c.MaxTaint(); t > max {
			max = t
		}
	}
	return max
}

// AllStrings 深度优先收集所有 string 叶子的内容。
// 供 Spotlighting 围栏标记和 TaintTracker 打标使用。
func (n *TaintedJSONNode) AllStrings() []string {
	if n == nil {
		return nil
	}
	if n.Kind == kindString {
		return []string{n.StrVal}
	}
	var result []string
	for _, c := range n.ArrNodes {
		result = append(result, c.AllStrings()...)
	}
	for _, c := range n.ObjNodes {
		result = append(result, c.AllStrings()...)
	}
	return result
}

// TaintPreservingDecoder 对 MCP JSON-RPC 动态嵌套响应进行污点保护反序列化。
//
// 安全背景: 默认 encoding/json 解析 MCP 响应为 map[string]interface{}
// 会丢失污点（违反 M11 §2.1 第四重防护）。本解码器递归遍历 JSON 树，
// 对每个 string 叶子按 M11 §2.4 [Connector-Taint-Table] 打标：
//   - 白名单 MCP server → TaintMedium
//   - 其余 → TaintHigh（保守）
//
// 非 string（number/bool/null）不包装，仅保留 JSON 路径溯源。
//
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §1
type TaintPreservingDecoder struct {
	serverName string
	taint      protocol.TaintLevel
}

// NewTaintPreservingDecoder 创建指定 MCP server 的污点保护解码器。
//
// trusted=true → 白名单（TaintMedium）；
// trusted=false → TaintHigh（外部 server，保守处理）。
func NewTaintPreservingDecoder(serverName string, trusted bool) *TaintPreservingDecoder {
	taint := protocol.TaintHigh
	if trusted {
		taint = protocol.TaintMedium
	}
	return &TaintPreservingDecoder{serverName: serverName, taint: taint}
}

// Taint 返回此解码器对应的初始污点等级（受 trusted 标志决定）。
func (d *TaintPreservingDecoder) Taint() protocol.TaintLevel {
	return d.taint
}

// Decode 对 MCP 响应原始 JSON 进行污点标注遍历。
//
// 解析失败的内容视为不可信字符串，按 TaintHigh 保守处理。
// path 为起始 JSON 路径（根节点传 ""）。
func (d *TaintPreservingDecoder) Decode(raw json.RawMessage, path string) *TaintedJSONNode {
	if len(raw) == 0 {
		return &TaintedJSONNode{Kind: kindNull, JSONPath: path}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 解析失败：保守按 TaintHigh 处理
		return &TaintedJSONNode{
			Kind:     kindString,
			StrVal:   string(raw),
			Taint:    protocol.TaintHigh,
			JSONPath: path,
		}
	}
	return d.walk(v, path)
}

// walk 递归遍历 any 值，构建带污点的节点树。
func (d *TaintPreservingDecoder) walk(v any, path string) *TaintedJSONNode {
	switch val := v.(type) {
	case nil:
		return &TaintedJSONNode{Kind: kindNull, JSONPath: path}

	case string:
		// string 叶子：打标。这是 TaintPreservingDecoder 的核心能力。
		return &TaintedJSONNode{
			Kind:     kindString,
			StrVal:   val,
			Taint:    d.taint,
			JSONPath: path,
		}

	case float64:
		// number 不包装，Taint 保持 TaintNone
		return &TaintedJSONNode{Kind: kindNumber, NumVal: val, JSONPath: path}

	case bool:
		return &TaintedJSONNode{Kind: kindBool, BoolVal: val, JSONPath: path}

	case []any:
		node := &TaintedJSONNode{Kind: kindArray, JSONPath: path}
		for i, item := range val {
			child := d.walk(item, fmt.Sprintf("%s[%d]", path, i))
			node.ArrNodes = append(node.ArrNodes, child)
		}
		// 数组节点 Taint 取子树最高值
		node.Taint = node.MaxTaint()
		return node

	case map[string]any:
		node := &TaintedJSONNode{
			Kind:     kindObject,
			ObjNodes: make(map[string]*TaintedJSONNode, len(val)),
			JSONPath: path,
		}
		for k, item := range val {
			node.ObjNodes[k] = d.walk(item, fmt.Sprintf("%s.%s", path, k))
		}
		node.Taint = node.MaxTaint()
		return node

	default:
		// 未知类型保守处理
		return &TaintedJSONNode{Kind: kindNull, Taint: d.taint, JSONPath: path}
	}
}
