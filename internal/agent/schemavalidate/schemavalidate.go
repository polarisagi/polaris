// Package schemavalidate 为 M4 Agent Kernel 的 LLMFillEffect.SchemaRef 提供轻量
// 结构化 JSON 校验（GR-4-005 复核修复）。
//
// 背景：internal/protocol/interfaces_agent.go 中 LLMFillEffect.SchemaRef 字段的
// 注释自 2026-07 起就写着"→ spec/schemas.json"，但该文件从未存在，SchemaRef 被
// 各处 Transition 赋值（"perceive_task"/"plan_dag"/"reflect_result"/"l3_watchdog"）
// 后从未被任何代码读取消费——是一个只声明未实现的死字段。主线规划路径
// (fsm.parsePlanOnSuccess) 与 PRM 并发候选路径 (agent_execute_effect.go) 都是对
// LLM 返回文本直接 json.Unmarshal，字段类型错误/缺失时静默产生零值结构体，不会
// 报错，这正是 GR-4-005 指出的问题，且核实过程中发现比原报告描述更广——不是
// "PRM 路径比主线弱"，而是两条路径事实上都没有真正的 Schema 校验。
//
// 本包不引入完整的 JSON-Schema (draft-07) 规范实现（过度工程，YAGNI）：只实现
// 覆盖当前实际问题所需的最小子集——type / required / properties / items /
// enum，足以校验 DAGModel（plan_dag）和 ReflectionModel（reflect_result）这类
// "顶层 object + 若干具名字段"的结构。
//
// "l3_watchdog" 的 LLM 响应约定是纯文本前缀（"ALLOW"/"DENY: ..."），本质不是
// JSON，不注册到 schemas.json 中；"perceive_task" 的 OnSuccess
// (fsm.onPerceiveSuccess) 目前完全不解析 fill 内容，同样不注册。Validate 对未
// 注册的 schemaRef 直接放行（不是 fail-closed 的安全边界，是"尚未定义校验规则"，
// 语义上等价于跳过，而不是拒绝）。
package schemavalidate

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

//go:embed schemas.json
var schemasFS embed.FS

// fieldSchema 是本包支持的 JSON Schema 子集（type/required/properties/items/enum）。
type fieldSchema struct {
	Type       string                  `json:"type"`
	Required   []string                `json:"required,omitempty"`
	Properties map[string]*fieldSchema `json:"properties,omitempty"`
	Items      *fieldSchema            `json:"items,omitempty"`
	Enum       []string                `json:"enum,omitempty"`
}

// getRegistry 惰性解析内嵌的 schemas.json（只读单次计算，CLAUDE.md 禁全局可变变量
// 的豁免类别之一，写法与 internal/security/network/safe_dialer.go 的
// getParsedCIDRs 保持一致）。schemas.json 是编译期内嵌的静态资源，解析失败只可能
// 是构建期本身出了问题（文件损坏/格式错误），此时 panic 在包初始化阶段就会暴露，
// 优于把一个必然失败的状态一路悄悄传播到运行时才被发现。
//
//nolint:gochecknoglobals // sync.OnceValue 惰性只读单例，非裸可变全局状态（同 safe_dialer.go 先例）
var getRegistry = sync.OnceValue(func() map[string]*fieldSchema {
	b, err := schemasFS.ReadFile("schemas.json")
	if err != nil {
		panic("schemavalidate: failed to read embedded schemas.json: " + err.Error())
	}
	var m map[string]*fieldSchema
	if err := json.Unmarshal(b, &m); err != nil {
		panic("schemavalidate: failed to parse embedded schemas.json: " + err.Error())
	}
	return m
})

// Validate 校验 raw（LLM 返回的原始 JSON 字节）是否符合 schemaRef 对应的结构定义。
//
//   - schemaRef 为空，或未在 schemas.json 中注册：直接放行（视为"未定义校验规则"，
//     不是安全边界，调用方不应据此认为内容一定合法）。
//   - raw 不是合法 JSON：返回错误（这一层职责与原有的 json.Unmarshal 错误处理重叠，
//     调用方通常在 Unmarshal 失败时已经短路，这里是双重防御，不冲突）。
//   - JSON 合法但不满足 required/type/enum 约束：返回带具体字段路径的错误。
func Validate(schemaRef string, raw []byte) error {
	if schemaRef == "" {
		return nil
	}
	schema, ok := getRegistry()[schemaRef]
	if !ok {
		return nil
	}

	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("schemavalidate: %s: invalid JSON", schemaRef), err)
	}

	if err := validateValue(schemaRef, data, schema); err != nil {
		return err
	}
	return nil
}

func validateValue(path string, data any, schema *fieldSchema) error {
	if schema == nil {
		return nil
	}
	if err := checkType(path, data, schema.Type); err != nil {
		return err
	}
	if err := checkEnum(path, data, schema); err != nil {
		return err
	}

	switch schema.Type {
	case "object":
		return validateObject(path, data, schema)
	case "array":
		return validateArray(path, data, schema)
	default:
		return nil
	}
}

func checkEnum(path string, data any, schema *fieldSchema) error {
	if len(schema.Enum) == 0 {
		return nil
	}
	s, ok := data.(string)
	if !ok || !contains(schema.Enum, s) {
		return apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("schemavalidate: %s: value %v not in enum %v", path, data, schema.Enum))
	}
	return nil
}

func validateObject(path string, data any, schema *fieldSchema) error {
	obj, _ := data.(map[string]any)
	for _, req := range schema.Required {
		if _, exists := obj[req]; !exists {
			return apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("schemavalidate: %s: missing required field %q", path, req))
		}
	}
	for fieldName, fieldSchema := range schema.Properties {
		v, exists := obj[fieldName]
		if !exists {
			continue // 非 required 字段允许缺省
		}
		if err := validateValue(path+"."+fieldName, v, fieldSchema); err != nil {
			return err
		}
	}
	return nil
}

func validateArray(path string, data any, schema *fieldSchema) error {
	arr, _ := data.([]any)
	if schema.Items == nil {
		return nil
	}
	for i, item := range arr {
		if err := validateValue(fmt.Sprintf("%s[%d]", path, i), item, schema.Items); err != nil {
			return err
		}
	}
	return nil
}

// checkType 校验 JSON 解码后的 Go 值（object→map[string]any, array→[]any,
// string→string, number→float64, boolean→bool）是否匹配声明的 type。
// type 为空表示不限制类型（仅做子字段/元素级校验）。
func checkType(path string, data any, wantType string) error {
	if wantType == "" {
		return nil
	}
	ok := false
	switch wantType {
	case "object":
		_, ok = data.(map[string]any)
	case "array":
		_, ok = data.([]any)
	case "string":
		_, ok = data.(string)
	case "number", "integer":
		_, ok = data.(float64)
	case "boolean":
		_, ok = data.(bool)
	default:
		ok = true // 未知声明类型不拦截，避免 schemas.json 笔误导致误伤所有 LLM 输出
	}
	if !ok {
		return apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("schemavalidate: %s: expected type %q, got %T", path, wantType, data))
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}
