package sysadmin

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestParseReasoningEffort(t *testing.T) {
	cases := []struct {
		in  string
		out types.ReasoningEffort
	}{
		{"low", types.ReasoningEffortLow},
		{"medium", types.ReasoningEffortMedium},
		{"high", types.ReasoningEffortHigh},
		{"ultra", types.ReasoningEffortHigh},
		{"unknown", types.ReasoningEffortMedium},
		{"", types.ReasoningEffortMedium},
	}

	for _, c := range cases {
		if res := parseReasoningEffort(c.in); res != c.out {
			t.Errorf("parseReasoningEffort(%q) = %v, expected %v", c.in, res, c.out)
		}
	}
}

func TestParseCronField(t *testing.T) {
	step, fixed := parseCronField("*")
	if step != 1 || fixed != -1 {
		t.Errorf("expected 1, -1 for *")
	}

	step, fixed = parseCronField("*/5")
	if step != 5 || fixed != -1 {
		t.Errorf("expected 5, -1 for */5")
	}

	step, fixed = parseCronField("*/abc")
	if step != 1 || fixed != -1 {
		t.Errorf("expected fallback 1, -1 for */abc")
	}

	step, fixed = parseCronField("15")
	if step != 1 || fixed != 15 {
		t.Errorf("expected 1, 15 for 15")
	}

	step, fixed = parseCronField("abc")
	if step != 1 || fixed != -1 {
		t.Errorf("expected fallback 1, -1 for abc")
	}
}

func TestCalcNextRun(t *testing.T) {
	from := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)

	next := calcNextRun("@hourly", from)
	expected := time.Date(2023, 1, 1, 13, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if next != expected {
		t.Errorf("expected %s for @hourly, got %s", expected, next)
	}

	next = calcNextRun("@daily", from)
	expected = time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if next != expected {
		t.Errorf("expected %s for @daily, got %s", expected, next)
	}

	next = calcNextRun("bad_expr", from)
	if next != "" {
		t.Errorf("expected empty string for bad expr")
	}

	// 6 fields fallback
	next = calcNextRun("0 0 * * * *", from)
	if next == "" {
		t.Errorf("expected non-empty for 6 fields")
	}
}

func TestMatchEventFilter(t *testing.T) {
	filter := `{"topic": "a", "type": "b"}`
	if !matchEventFilter(filter, "a", "b", "") {
		t.Errorf("expected true")
	}
	if matchEventFilter(filter, "b", "b", "") {
		t.Errorf("expected false")
	}
	if matchEventFilter(`invalid json`, "a", "b", "") {
		t.Errorf("expected false for invalid json")
	}
}
