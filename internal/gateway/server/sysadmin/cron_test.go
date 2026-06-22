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
	tests := []struct {
		name       string
		filterJSON string
		topic      string
		typ        string
		payload    string
		want       bool
	}{
		{
			name:       "no payload condition (should match)",
			filterJSON: `{"topic":"test-topic","type":"test-type"}`,
			topic:      "test-topic",
			typ:        "test-type",
			payload:    `{"any":"thing"}`,
			want:       true,
		},
		{
			name:       "payload condition met (should match)",
			filterJSON: `{"topic":"test-topic","payload":{"foo":"bar","num":123}}`,
			topic:      "test-topic",
			typ:        "any-type",
			payload:    `{"foo":"bar","num":123,"extra":"value"}`,
			want:       true,
		},
		{
			name:       "payload condition not met (should not match)",
			filterJSON: `{"topic":"test-topic","payload":{"foo":"bar"}}`,
			topic:      "test-topic",
			typ:        "any-type",
			payload:    `{"foo":"baz"}`,
			want:       false,
		},
		{
			name:       "payload condition missing key (should not match)",
			filterJSON: `{"payload":{"required":"yes"}}`,
			topic:      "any",
			typ:        "any",
			payload:    `{"other":"yes"}`,
			want:       false,
		},
		{
			name:       "payload not JSON when filter has payload condition (should not match)",
			filterJSON: `{"payload":{"foo":"bar"}}`,
			topic:      "any",
			typ:        "any",
			payload:    `not-json`,
			want:       false,
		},
		{
			name:       "payload not JSON but no payload condition (should match)",
			filterJSON: `{"topic":"test-topic"}`,
			topic:      "test-topic",
			typ:        "any",
			payload:    `not-json`,
			want:       true,
		},
		{
			name:       "type mismatch",
			filterJSON: `{"type":"type1"}`,
			topic:      "any",
			typ:        "type2",
			payload:    `{}`,
			want:       false,
		},
		{
			name:       "topic mismatch",
			filterJSON: `{"topic":"topic1"}`,
			topic:      "topic2",
			typ:        "any",
			payload:    `{}`,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchEventFilter(tt.filterJSON, tt.topic, tt.typ, tt.payload)
			if got != tt.want {
				t.Errorf("matchEventFilter() = %v, want %v", got, tt.want)
			}
		})
	}
}
