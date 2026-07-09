package csv_parse

import (
	"testing"
)

func TestSplitCSVLine(t *testing.T) {
	line := `a,"b,c",d,"""e"""`
	fields := splitCSVLine(line)
	if len(fields) != 4 {
		t.Fatalf("expected 4 fields, got %d", len(fields))
	}
	if fields[0] != "a" {
		t.Errorf("field 0 mismatch")
	}
	if fields[1] != "b,c" {
		t.Errorf("field 1 mismatch")
	}
	if fields[2] != "d" {
		t.Errorf("field 2 mismatch")
	}
	if fields[3] != "\"e\"" {
		t.Errorf("field 3 mismatch")
	}
}
