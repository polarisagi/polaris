package elicitation

import (
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func mustSchema(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestValidateFormContent_RequiredMissing(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	err := validateFormContent(schema, map[string]any{})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput, got %v", err)
	}
}

func TestValidateFormContent_UnknownProperty(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"name":{"type":"string"}}}`)
	err := validateFormContent(schema, map[string]any{"age": 1})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput for unknown property, got %v", err)
	}
}

func TestValidateFormContent_StringTypeMismatch(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"name":{"type":"string"}}}`)
	err := validateFormContent(schema, map[string]any{"name": 123.0})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput, got %v", err)
	}
}

func TestValidateFormContent_StringLengthBounds(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"name":{"type":"string","minLength":3,"maxLength":5}}}`)
	if err := validateFormContent(schema, map[string]any{"name": "ab"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected minLength violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"name": "abcdef"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected maxLength violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"name": "abcd"}); err != nil {
		t.Fatalf("unexpected error for valid length: %v", err)
	}
}

func TestValidateFormContent_NumberBounds(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"age":{"type":"number","minimum":18,"maximum":30}}}`)
	if err := validateFormContent(schema, map[string]any{"age": 10.0}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected minimum violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"age": 40.0}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected maximum violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"age": 25.0}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_IntegerRejectsFraction(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"count":{"type":"integer"}}}`)
	if err := validateFormContent(schema, map[string]any{"count": 1.5}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected integer violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"count": 3.0}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_Boolean(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	if err := validateFormContent(schema, map[string]any{"ok": "yes"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected boolean type violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"ok": true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_SingleSelectEnum(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"color":{"type":"string","enum":["Red","Green","Blue"]}}}`)
	if err := validateFormContent(schema, map[string]any{"color": "Purple"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected enum violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"color": "Red"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_SingleSelectOneOfWithTitles(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"color":{"type":"string","oneOf":[{"const":"#FF0000","title":"Red"},{"const":"#00FF00","title":"Green"}]}}}`)
	if err := validateFormContent(schema, map[string]any{"color": "#0000FF"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected oneOf violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"color": "#FF0000"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_MultiSelectEnumWithoutTitles(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"colors":{"type":"array","minItems":1,"maxItems":2,"items":{"type":"string","enum":["Red","Green","Blue"]}}}}`)
	if err := validateFormContent(schema, map[string]any{"colors": []any{}}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected minItems violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"colors": []any{"Red", "Green", "Blue"}}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected maxItems violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"colors": []any{"Purple"}}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected item enum violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"colors": []any{"Red", "Green"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_MultiSelectEnumWithTitles(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"colors":{"type":"array","items":{"anyOf":[{"const":"#FF0000","title":"Red"},{"const":"#00FF00","title":"Green"}]}}}}`)
	if err := validateFormContent(schema, map[string]any{"colors": []any{"#0000FF"}}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected anyOf violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"colors": []any{"#FF0000", "#00FF00"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_FormatEmail(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"email":{"type":"string","format":"email"}}}`)
	if err := validateFormContent(schema, map[string]any{"email": "not-an-email"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected email format violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"email": "octocat@github.com"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_FormatURI(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"site":{"type":"string","format":"uri"}}}`)
	if err := validateFormContent(schema, map[string]any{"site": "not a uri"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected uri format violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"site": "https://example.com"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_FormatDate(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"d":{"type":"string","format":"date"}}}`)
	if err := validateFormContent(schema, map[string]any{"d": "2026-13-40"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected date format violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"d": "2026-09-27"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_FormatDateTime(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"d":{"type":"string","format":"date-time"}}}`)
	if err := validateFormContent(schema, map[string]any{"d": "not-a-datetime"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected date-time format violation, got %v", err)
	}
	if err := validateFormContent(schema, map[string]any{"d": "2026-09-27T10:00:00Z"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFormContent_NestedObjectPropertyRejected(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"addr":{"type":"object"}}}`)
	if err := validateFormContent(schema, map[string]any{"addr": map[string]any{"city": "x"}}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected unsupported type violation, got %v", err)
	}
}
