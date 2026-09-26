package elicitation

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// elicitSchema requestedSchema 顶层结构（spec_elicitation.md §Requested Schema：
// 平铺 object + primitive 属性，internal/extension/mcp 已校验过形状，这里独立
// 复核一次并额外做取值校验——两处各自独立不互相信任，防止其中一处校验退化后
// 未受校验的内容悄悄流通，见 M07 §权力边界"禁止静默丢弃 Schema 校验失败"）。
type elicitSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

// elicitProperty 单个属性的完整可判定形状（较 mcp 包的形状校验多出取值约束字段）。
type elicitProperty struct {
	Type      string          `json:"type"`
	Format    string          `json:"format"`
	MinLength *int            `json:"minLength"`
	MaxLength *int            `json:"maxLength"`
	Minimum   *float64        `json:"minimum"`
	Maximum   *float64        `json:"maximum"`
	Enum      []any           `json:"enum"`
	OneOf     []constOption   `json:"oneOf"`
	Items     json.RawMessage `json:"items"`
	MinItems  *int            `json:"minItems"`
	MaxItems  *int            `json:"maxItems"`
}

// constOption oneOf/anyOf 的 {const,title} 变体（单选/多选枚举「带标题」形式）。
type constOption struct {
	Const any `json:"const"`
}

// elicitItems 多选 enum 的 items 形状（§Requested Schema「Multi-select enum」两种变体）。
type elicitItems struct {
	Type  string        `json:"type"`
	Enum  []any         `json:"enum"`
	AnyOf []constOption `json:"anyOf"`
	OneOf []constOption `json:"oneOf"`
}

// validateFormContent 校验 content 是否匹配 requestedSchema：只允许声明的属性、
// required 属性必须存在、每个出现的属性值须匹配其类型与约束。
func validateFormContent(schemaRaw json.RawMessage, content map[string]any) error {
	schema, err := parseElicitSchema(schemaRaw)
	if err != nil {
		return err
	}
	if err := checkUnknownProperties(schema, content); err != nil {
		return err
	}
	if err := checkRequiredProperties(schema, content); err != nil {
		return err
	}
	for name, propRaw := range schema.Properties {
		val, present := content[name]
		if !present {
			continue
		}
		if err := validatePropertyValue(name, propRaw, val); err != nil {
			return err
		}
	}
	return nil
}

func parseElicitSchema(raw json.RawMessage) (elicitSchema, error) {
	var schema elicitSchema
	if len(raw) == 0 {
		return schema, apperr.New(apperr.CodeInvalidInput, "elicitation: requestedSchema 缺失")
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return schema, apperr.Wrap(apperr.CodeInvalidInput, "elicitation: requestedSchema 解析失败", err)
	}
	return schema, nil
}

func checkUnknownProperties(schema elicitSchema, content map[string]any) error {
	for name := range content {
		if _, ok := schema.Properties[name]; !ok {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: content 含未声明属性 %q", name))
		}
	}
	return nil
}

func checkRequiredProperties(schema elicitSchema, content map[string]any) error {
	for _, name := range schema.Required {
		if _, ok := content[name]; !ok {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 缺少必填属性 %q", name))
		}
	}
	return nil
}

func validatePropertyValue(name string, propRaw json.RawMessage, val any) error {
	var p elicitProperty
	if err := json.Unmarshal(propRaw, &p); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q schema 解析失败", name), err)
	}
	switch p.Type {
	case "string":
		return validateStringValue(name, p, val)
	case "number", "integer":
		return validateNumberValue(name, p, val)
	case "boolean":
		return validateBooleanValue(name, val)
	case "array":
		return validateArrayValue(name, p, val)
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 类型 %q 不受支持", name, p.Type))
	}
}

func validateStringValue(name string, p elicitProperty, val any) error {
	s, ok := val.(string)
	if !ok {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 期望字符串", name))
	}
	if p.MinLength != nil && len(s) < *p.MinLength {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 短于 minLength", name))
	}
	if p.MaxLength != nil && len(s) > *p.MaxLength {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 长于 maxLength", name))
	}
	if err := validateStringEnum(name, p, s); err != nil {
		return err
	}
	return validateStringFormat(name, p.Format, s)
}

func validateStringEnum(name string, p elicitProperty, s string) error {
	if len(p.Enum) > 0 && !containsAny(p.Enum, s) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不在 enum 取值范围内", name))
	}
	if len(p.OneOf) > 0 && !containsConst(p.OneOf, s) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不在 oneOf 取值范围内", name))
	}
	return nil
}

// validateStringFormat 校验 spec_elicitation.md §Requested Schema 列出的四种
// format：email/uri/date/date-time。未声明 format 或声明了规范未列出的值一律放行
// （宽松兜底，与 internal/extension/mcp 的形状校验一致不做过度收紧）。
func validateStringFormat(name, format, s string) error {
	switch format {
	case "email":
		if _, err := mail.ParseAddress(s); err != nil {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不是合法 email", name))
		}
	case "uri":
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不是合法 uri", name))
		}
	case "date":
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不是合法 date", name))
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 不是合法 date-time", name))
		}
	}
	return nil
}

func validateNumberValue(name string, p elicitProperty, val any) error {
	n, ok := toFloat64(val)
	if !ok {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 期望数字", name))
	}
	if p.Type == "integer" && n != math.Trunc(n) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 期望整数", name))
	}
	if p.Minimum != nil && n < *p.Minimum {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 小于 minimum", name))
	}
	if p.Maximum != nil && n > *p.Maximum {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 大于 maximum", name))
	}
	return nil
}

func validateBooleanValue(name string, val any) error {
	if _, ok := val.(bool); !ok {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 期望布尔值", name))
	}
	return nil
}

// validateArrayValue 多选 enum 属性（§Requested Schema「Multi-select enum」）。
func validateArrayValue(name string, p elicitProperty, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 期望数组（多选 enum）", name))
	}
	if p.MinItems != nil && len(arr) < *p.MinItems {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素数量少于 minItems", name))
	}
	if p.MaxItems != nil && len(arr) > *p.MaxItems {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素数量多于 maxItems", name))
	}
	var items elicitItems
	if err := json.Unmarshal(p.Items, &items); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q items schema 解析失败", name), err)
	}
	for _, elem := range arr {
		if err := validateArrayItem(name, items, elem); err != nil {
			return err
		}
	}
	return nil
}

func validateArrayItem(name string, items elicitItems, elem any) error {
	switch {
	case items.Type != "":
		if len(items.Enum) > 0 && !containsAny(items.Enum, elem) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素不在 enum 取值范围内", name))
		}
		if !valueMatchesType(items.Type, elem) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素类型与 items.type 不符", name))
		}
	case len(items.AnyOf) > 0:
		if !containsConst(items.AnyOf, elem) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素不在 anyOf 取值范围内", name))
		}
	case len(items.OneOf) > 0:
		if !containsConst(items.OneOf, elem) {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q 元素不在 oneOf 取值范围内", name))
		}
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: 属性 %q items schema 不受支持", name))
	}
	return nil
}

func valueMatchesType(t string, val any) bool {
	switch t {
	case "string":
		_, ok := val.(string)
		return ok
	case "number", "integer":
		_, ok := toFloat64(val)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	default:
		return false
	}
}

// toFloat64 content 经 JSON 解码后数字统一为 float64；额外兼容测试/内部构造场景下
// 直接传入 int 的情形。
func toFloat64(val any) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

// containsAny 判断 val 是否等于 list 中某一项（JSON 解码后的可比较标量类型：
// string/float64/bool，interface 相等语义天然成立）。
func containsAny(list []any, val any) bool {
	for _, item := range list {
		if item == val {
			return true
		}
	}
	return false
}

func containsConst(options []constOption, val any) bool {
	for _, o := range options {
		if o.Const == val {
			return true
		}
	}
	return false
}
