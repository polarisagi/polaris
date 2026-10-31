package schemavalidate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ValidateAgainstStrict 用有限且明确的 JSON Schema 子集拒绝无法证明的输入。
func ValidateAgainstStrict(schema any, raw []byte) error {
	if schema == nil {
		return strictSchemaError("schema is nil")
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "schemavalidate: schema is not JSON serializable", err)
	}
	if err := validateStrictSchemaDefinition("$", json.RawMessage(b)); err != nil {
		return err
	}
	var data any
	if err := decodeStrictJSON(raw, &data); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "schemavalidate: invalid input JSON", err)
	}
	if err := validateStrictSchemaValue("$", data, json.RawMessage(b)); err != nil {
		return err
	}
	return nil
}

//nolint:gocyclo // 单一入口枚举受支持的Schema子集，拆开会分散fail-closed规则。
func validateStrictSchemaDefinition(path string, raw json.RawMessage) error {
	fields, err := parseStrictSchema(path, raw)
	if err != nil {
		return err
	}
	if kind, ok := fields["type"]; ok {
		var name string
		if err := json.Unmarshal(kind, &name); err != nil || !supportedStrictType(name) {
			return strictSchemaError(path + ": type must name a supported scalar type")
		}
	}
	if required, ok := fields["required"]; ok {
		var names []string
		if len(required) == 0 || required[0] != '[' {
			return strictSchemaError(path + ": required must be an array of strings")
		}
		if err := json.Unmarshal(required, &names); err != nil || names == nil {
			return strictSchemaError(path + ": required must be an array of strings")
		}
	}
	if enum, ok := fields["enum"]; ok {
		var values []json.RawMessage
		if len(enum) == 0 || enum[0] != '[' || json.Unmarshal(enum, &values) != nil || len(values) == 0 {
			return strictSchemaError(path + ": enum must be a non-empty array")
		}
	}
	for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		if bound, ok := fields[key]; ok {
			var number json.Number
			if err := json.Unmarshal(bound, &number); err != nil || number == "" {
				return strictSchemaError(path + ": " + key + " must be a number")
			}
			if _, ok := new(big.Rat).SetString(string(number)); !ok {
				return strictSchemaError(path + ": " + key + " is invalid")
			}
		}
	}
	if properties, ok := fields["properties"]; ok { //nolint:nestif // 子Schema必须递归按同一有限子集校验。
		var children map[string]json.RawMessage
		if json.Unmarshal(properties, &children) != nil || children == nil {
			return strictSchemaError(path + ": properties must be an object")
		}
		for name, child := range children {
			if err := validateStrictSchemaDefinition(path+"."+name, child); err != nil {
				return err
			}
		}
	}
	if items, ok := fields["items"]; ok { //nolint:nestif // 数组子Schema递归共享同一拒绝规则。
		if len(items) == 0 || items[0] != '{' {
			return apperr.New(apperr.CodeUnimplemented, "schemavalidate: "+path+": items must be a schema object")
		}
		if err := validateStrictSchemaDefinition(path+"[]", items); err != nil {
			return err
		}
	}
	return nil
}

func decodeStrictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "schemavalidate: decode JSON", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return apperr.New(apperr.CodeInvalidInput, "schemavalidate: multiple JSON values")
		}
		return apperr.Wrap(apperr.CodeInvalidInput, "schemavalidate: trailing JSON", err)
	}
	return nil
}

//nolint:gocyclo // 类型、枚举、约束与递归字段须在一次值校验中共同fail-closed。
func validateStrictSchemaValue(path string, value any, raw json.RawMessage) error {
	fields, err := parseStrictSchema(path, raw)
	if err != nil {
		return err
	}
	if want, ok := fields["type"]; ok {
		var wantType string
		if err := json.Unmarshal(want, &wantType); err != nil {
			return strictSchemaError(path + ": type must be a string")
		}
		if !supportedStrictType(wantType) {
			return strictSchemaError(fmt.Sprintf("%s: unsupported type %q", path, wantType))
		}
		if !strictTypeMatches(value, wantType) {
			return strictInputError(fmt.Sprintf("%s: expected type %q, got %T", path, wantType, value))
		}
	}
	if enum, ok := fields["enum"]; ok {
		var values []json.RawMessage
		if err := json.Unmarshal(enum, &values); err != nil || len(values) == 0 {
			return strictSchemaError(path + ": enum must be a non-empty array")
		}
		matched := false
		for _, candidate := range values {
			var decoded any
			if err := decodeStrictJSON(candidate, &decoded); err != nil {
				return strictSchemaError(path + ": enum contains invalid JSON")
			}
			if strictJSONEqual(value, decoded) {
				matched = true
				break
			}
		}
		if !matched {
			return strictInputError(path + ": value is not in enum")
		}
	}
	if err := validateStrictNumber(path, value, fields); err != nil {
		return err
	}
	if required, ok := fields["required"]; ok {
		var names []string
		if err := json.Unmarshal(required, &names); err != nil {
			return strictSchemaError(path + ": required must be an array of strings")
		}
		if object, ok := value.(map[string]any); ok {
			for _, name := range names {
				if _, exists := object[name]; !exists {
					return strictInputError(fmt.Sprintf("%s: missing required field %q", path, name))
				}
			}
		}
	}
	if properties, ok := fields["properties"]; ok {
		if err := validateStrictProperties(path, value, properties); err != nil {
			return err
		}
	}
	if items, ok := fields["items"]; ok {
		if err := validateStrictItems(path, value, items); err != nil {
			return err
		}
	}
	return nil
}

func validateStrictProperties(path string, value any, raw json.RawMessage) error {
	var children map[string]json.RawMessage
	if err := json.Unmarshal(raw, &children); err != nil || children == nil {
		return strictSchemaError(path + ": properties must be an object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child, exists := object[name]
		if !exists {
			continue
		}
		if err := validateStrictSchemaValue(path+"."+name, child, children[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateStrictItems(path string, value any, raw json.RawMessage) error {
	var tuple []any
	if err := json.Unmarshal(raw, &tuple); err == nil {
		return strictSchemaError(path + ": tuple items are unsupported")
	}
	if _, err := parseStrictSchema(path+"[]", raw); err != nil {
		return err
	}
	array, ok := value.([]any)
	if !ok {
		return nil
	}
	for index, child := range array {
		if err := validateStrictSchemaValue(fmt.Sprintf("%s[%d]", path, index), child, raw); err != nil {
			return err
		}
	}
	return nil
}

//nolint:gocyclo // 精确JSON相等按类型、对象、数组递归比较。
func strictJSONEqual(left, right any) bool {
	if leftNumber, ok := left.(json.Number); ok {
		rightNumber, ok := right.(json.Number)
		if !ok {
			return false
		}
		leftRat, leftOK := new(big.Rat).SetString(string(leftNumber))
		rightRat, rightOK := new(big.Rat).SetString(string(rightNumber))
		return leftOK && rightOK && leftRat.Cmp(rightRat) == 0
	}
	if leftObject, ok := left.(map[string]any); ok {
		rightObject, ok := right.(map[string]any)
		if !ok || len(leftObject) != len(rightObject) {
			return false
		}
		for key, leftValue := range leftObject {
			rightValue, exists := rightObject[key]
			if !exists || !strictJSONEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	}
	if leftArray, ok := left.([]any); ok {
		rightArray, ok := right.([]any)
		if !ok || len(leftArray) != len(rightArray) {
			return false
		}
		for index := range leftArray {
			if !strictJSONEqual(leftArray[index], rightArray[index]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(left, right)
}

func parseStrictSchema(path string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, strictSchemaError(path + ": schema must be an object")
	}
	allowed := map[string]bool{
		"type": true, "required": true, "properties": true, "items": true, "enum": true,
		"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
		"title": true, "description": true, "$comment": true, "$schema": true, "$id": true,
		"default": true, "examples": true,
	}
	for key := range fields {
		if !allowed[key] {
			return nil, apperr.New(apperr.CodeUnimplemented,
				fmt.Sprintf("schemavalidate: %s: unsupported schema keyword %q", path, key))
		}
	}
	return fields, nil
}

func supportedStrictType(kind string) bool {
	switch kind {
	case "object", "array", "string", "number", "integer", "boolean", "null":
		return true
	default:
		return false
	}
}

func strictTypeMatches(value any, kind string) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		r, ok := new(big.Rat).SetString(string(n))
		return ok && r.IsInt()
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

//nolint:gocyclo // 四种数值边界共享精确有理数比较，集中处理避免语义漂移。
func validateStrictNumber(path string, value any, fields map[string]json.RawMessage) error {
	number, ok := value.(json.Number)
	if !ok {
		for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
			if _, exists := fields[key]; exists {
				return strictInputError(path + ": numeric constraint applied to non-number")
			}
		}
		return nil
	}
	actual, ok := new(big.Rat).SetString(string(number))
	if !ok {
		return strictInputError(path + ": invalid number")
	}
	for key, relation := range map[string]int{"minimum": 1, "maximum": -1, "exclusiveMinimum": 2, "exclusiveMaximum": -2} {
		boundRaw, exists := fields[key]
		if !exists {
			continue
		}
		var boundNumber json.Number
		if err := json.Unmarshal(boundRaw, &boundNumber); err != nil {
			return strictSchemaError(path + ": " + key + " must be a number")
		}
		bound, ok := new(big.Rat).SetString(string(boundNumber))
		if !ok {
			return strictSchemaError(path + ": " + key + " is invalid")
		}
		cmp := actual.Cmp(bound)
		if (relation == 1 && cmp < 0) || (relation == -1 && cmp > 0) ||
			(relation == 2 && cmp <= 0) || (relation == -2 && cmp >= 0) {
			return strictInputError(path + ": number violates " + key)
		}
	}
	return nil
}

func strictSchemaError(message string) error {
	return apperr.New(apperr.CodeInvalidInput, "schemavalidate: invalid strict schema: "+message)
}

func strictInputError(message string) error {
	return apperr.New(apperr.CodeInvalidInput, "schemavalidate: "+message)
}
