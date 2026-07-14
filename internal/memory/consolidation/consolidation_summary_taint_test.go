package consolidation

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestSummaryTaintLevel_AllTaintNoneStaysNone(t *testing.T) {
	events := []types.ScoredEvent{
		{Event: &types.Event{TaintLevel: types.TaintNone}},
		{Event: &types.Event{TaintLevel: types.TaintNone}},
	}
	if got := summaryTaintLevel(events); got != types.TaintNone {
		t.Errorf("expected TaintNone when all events are TaintNone, got %v", got)
	}
}

func TestSummaryTaintLevel_AnyTaintedEventForcesMediumFloor(t *testing.T) {
	cases := []types.TaintLevel{types.TaintLow, types.TaintMedium, types.TaintHigh}
	for _, lvl := range cases {
		events := []types.ScoredEvent{
			{Event: &types.Event{TaintLevel: types.TaintNone}},
			{Event: &types.Event{TaintLevel: lvl}},
		}
		got := summaryTaintLevel(events)
		if got != types.TaintMedium {
			t.Errorf("input max=%v: expected SanitizeBySummarization hard floor TaintMedium, got %v", lvl, got)
		}
	}
}

func TestSummaryTaintLevel_EmptyEventsStaysNone(t *testing.T) {
	if got := summaryTaintLevel(nil); got != types.TaintNone {
		t.Errorf("expected TaintNone for empty events, got %v", got)
	}
}

func TestSummaryTaintLevel_NonEventPayloadIgnored(t *testing.T) {
	// se.Event 类型断言失败时应被安全忽略，不 panic、不影响其余事件的计算。
	events := []types.ScoredEvent{
		{Event: "not-an-event"},
		{Event: &types.Event{TaintLevel: types.TaintHigh}},
	}
	if got := summaryTaintLevel(events); got != types.TaintMedium {
		t.Errorf("expected TaintMedium floor from the valid tainted event, got %v", got)
	}
}
