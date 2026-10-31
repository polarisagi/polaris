package schemavalidate

import (
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestValidateAgainstStrict_ValidNestedSchema(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"path", "count", "mode", "values"},
		"properties": map[string]any{
			"path":   map[string]any{"type": "string"},
			"count":  map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
			"mode":   map[string]any{"enum": []any{"read", "write"}},
			"values": map[string]any{"type": "array", "items": map[string]any{"type": "number", "exclusiveMinimum": 0}},
		},
		"title": "ignored annotation",
	}
	raw := []byte(`{"path":"/tmp/a","count":10.0,"mode":"read","values":[0.1,2]}`)
	if err := ValidateAgainstStrict(schema, raw); err != nil {
		t.Fatalf("valid schema/input rejected: %v", err)
	}
}

func TestValidateAgainstStrict_RejectsUnsafeSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema any
	}{
		{name: "nil", schema: nil},
		{name: "unknown type", schema: map[string]any{"type": "file"}},
		{name: "type array", schema: map[string]any{"type": []any{"string", "null"}}},
		{name: "ref", schema: map[string]any{"$ref": "https://example.invalid/schema"}},
		{name: "anyOf", schema: map[string]any{"anyOf": []any{map[string]any{"type": "string"}}}},
		{name: "nested unsupported", schema: map[string]any{"properties": map[string]any{"x": map[string]any{"pattern": "x"}}}},
		{name: "invalid properties", schema: map[string]any{"properties": []any{"x"}}},
		{name: "non serializable", schema: map[string]any{"x": make(chan int)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateAgainstStrict(tt.schema, []byte(`{}`)); err == nil {
				t.Fatal("expected strict schema rejection")
			}
		})
	}
}

func TestValidateAgainstStrict_RejectsInvalidInputAndBounds(t *testing.T) {
	tests := []struct {
		name   string
		schema any
		raw    []byte
	}{
		{name: "invalid json", schema: map[string]any{"type": "object"}, raw: []byte(`{`)},
		{name: "empty raw", schema: map[string]any{"type": "object"}},
		{name: "missing required", schema: map[string]any{"type": "object", "required": []string{"id"}}, raw: []byte(`{}`)},
		{name: "wrong nested type", schema: map[string]any{"properties": map[string]any{"id": map[string]any{"type": "string"}}}, raw: []byte(`{"id":3}`)},
		{name: "fractional integer", schema: map[string]any{"type": "integer"}, raw: []byte(`1.5`)},
		{name: "large exact integer boundary", schema: map[string]any{"type": "integer", "maximum": json.Number("9223372036854775807")}, raw: []byte(`9223372036854775808`)},
		{name: "exclusive minimum", schema: map[string]any{"type": "number", "exclusiveMinimum": 2}, raw: []byte(`2`)},
		{name: "enum mismatch", schema: map[string]any{"enum": []any{"a", "b"}}, raw: []byte(`"c"`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateAgainstStrict(tt.schema, tt.raw); err == nil {
				t.Fatal("expected strict input rejection")
			}
		})
	}
}

func TestValidateAgainstStrict_EnumUsesJSONNumberEquality(t *testing.T) {
	schema := map[string]any{"enum": []any{json.Number("1.0")}}
	if err := ValidateAgainstStrict(schema, []byte(`1`)); err != nil {
		t.Fatalf("mathematically equal JSON numbers should match enum: %v", err)
	}
}

func TestValidateAgainstStrict_UnsupportedKeywordIsExplicit(t *testing.T) {
	err := ValidateAgainstStrict(map[string]any{"$ref": "#/$defs/tool"}, []byte(`{}`))
	if apperr.CodeOf(err) != apperr.CodeUnimplemented {
		t.Fatalf("unsupported constraint must be explicit, got %v (%s)", err, apperr.CodeOf(err))
	}
}

func TestValidateAgainstStrict_PreservesLooseValidatorCompatibility(t *testing.T) {
	if err := ValidateAgainst(map[string]any{"$ref": "#/$defs/tool"}, []byte(`{}`)); err != nil {
		t.Fatalf("legacy validator semantics changed: %v", err)
	}
	if err := ValidateAgainst(nil, []byte(`{}`)); err != nil {
		t.Fatalf("legacy nil schema should remain permissive: %v", err)
	}
}
