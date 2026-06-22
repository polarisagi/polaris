package action

import (
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestTaintPreservingDecoder_Decode(t *testing.T) {
	tests := []struct {
		name          string
		trusted       bool
		raw           string
		expectedTaint types.TaintLevel
	}{
		{
			name:          "Trusted JSON string",
			trusted:       true,
			raw:           `"hello"`,
			expectedTaint: types.TaintMedium,
		},
		{
			name:          "Untrusted JSON string",
			trusted:       false,
			raw:           `"world"`,
			expectedTaint: types.TaintHigh,
		},
		{
			name:          "Invalid JSON (fallback to string)",
			trusted:       false,
			raw:           `{invalid json`,
			expectedTaint: types.TaintHigh,
		},
		{
			name:          "Null JSON",
			trusted:       true,
			raw:           `null`,
			expectedTaint: types.TaintNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoder := NewTaintPreservingDecoder("test_server", tt.trusted)
			node := decoder.Decode(json.RawMessage(tt.raw), "$")

			if tt.name == "Null JSON" {
				if node.Taint != types.TaintNone {
					t.Errorf("expected taint %v, got %v", types.TaintNone, node.Taint)
				}
				return
			}

			if tt.name == "Invalid JSON (fallback to string)" {
				if node.Taint != tt.expectedTaint {
					t.Errorf("expected taint %v, got %v", tt.expectedTaint, node.Taint)
				}
				return
			}

			if node.MaxTaint() != tt.expectedTaint {
				t.Errorf("expected max taint %v, got %v", tt.expectedTaint, node.MaxTaint())
			}
		})
	}
}

func TestTaintPreservingDecoder_ComplexJSON(t *testing.T) {
	raw := []byte(`{
		"name": "test",
		"age": 30,
		"active": true,
		"tags": ["a", "b"],
		"nested": {
			"key": "value",
			"null_val": null
		}
	}`)
	decoder := NewTaintPreservingDecoder("test_server", false)
	node := decoder.Decode(raw, "$")

	if node.Kind != kindObject {
		t.Errorf("expected kindObject, got %v", node.Kind)
	}
	if node.MaxTaint() != types.TaintHigh {
		t.Errorf("expected MaxTaint %v, got %v", types.TaintHigh, node.MaxTaint())
	}

	strings := node.AllStrings()
	expectedStrings := map[string]bool{"test": true, "a": true, "b": true, "value": true}
	for _, s := range strings {
		if !expectedStrings[s] {
			t.Errorf("unexpected string found: %s", s)
		}
		delete(expectedStrings, s)
	}
	if len(expectedStrings) > 0 {
		t.Errorf("missing strings: %v", expectedStrings)
	}
}

func TestTaintPreservingDecoder_EmptyRaw(t *testing.T) {
	decoder := NewTaintPreservingDecoder("test_server", true)
	node := decoder.Decode(nil, "$")
	if node.Kind != kindNull {
		t.Errorf("expected kindNull, got %v", node.Kind)
	}
	if node.MaxTaint() != types.TaintNone {
		t.Errorf("expected TaintNone, got %v", node.MaxTaint())
	}
	if len(node.AllStrings()) != 0 {
		t.Errorf("expected empty string slice, got %v", node.AllStrings())
	}

	// cover AllStrings logic for nil node
	var n *TaintedJSONNode
	if len(n.AllStrings()) != 0 {
		t.Errorf("expected empty string slice for nil node")
	}
}

func TestTaintPreservingDecoder_Taint(t *testing.T) {
	decoder := NewTaintPreservingDecoder("s1", true)
	if decoder.Taint() != types.TaintMedium {
		t.Errorf("expected TaintMedium, got %v", decoder.Taint())
	}

	decoder2 := NewTaintPreservingDecoder("s2", false)
	if decoder2.Taint() != types.TaintHigh {
		t.Errorf("expected TaintHigh, got %v", decoder2.Taint())
	}
}

func TestTaintedJSONNode_AllStrings_StringNode(t *testing.T) {
	node := &TaintedJSONNode{
		Kind:   kindString,
		StrVal: "test",
	}
	strings := node.AllStrings()
	if len(strings) != 1 || strings[0] != "test" {
		t.Errorf("expected ['test'], got %v", strings)
	}
}

func TestTaintedJSONNode_WalkDefault(t *testing.T) {
	decoder := NewTaintPreservingDecoder("s1", true)
	// pass a channel to force default case in walk
	ch := make(chan int)
	node := decoder.walk(ch, "$")
	if node.Kind != kindNull {
		t.Errorf("expected kindNull, got %v", node.Kind)
	}
	if node.Taint != types.TaintMedium {
		t.Errorf("expected TaintMedium, got %v", node.Taint)
	}
}
