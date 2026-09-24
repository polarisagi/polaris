package fsm

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt"
)

func TestRecordObservation_Bounded(t *testing.T) {
	s := &StateContext{}
	for _, r := range []string{"r1", "r2", "r3", "r4", "r5"} {
		s.RecordObservation(r)
	}
	if len(s.Observations) != maxObservations || s.Observations[0] != "r2" {
		t.Fatalf("应只保留最近 %d 轮，得到 %v", maxObservations, s.Observations)
	}
	s.RecordObservation(strings.Repeat("x", maxObservationBytes*2))
	if last := s.Observations[len(s.Observations)-1]; len(last) > maxObservationBytes+32 {
		t.Fatalf("单轮观察应截断，得到 %d 字节", len(last))
	}
}

func TestWriteObservations(t *testing.T) {
	if WriteObservations(prompt.NewPromptBuilder(), &StateContext{}) {
		t.Fatal("无观察时不应写入")
	}
	b := prompt.NewPromptBuilder()
	s := &StateContext{}
	s.RecordObservation(`{"memory_total_gb":"16.00"}`)
	if !WriteObservations(b, s) {
		t.Fatal("有观察时应写入")
	}
	msgs := b.Build()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "memory_total_gb") || !strings.Contains(msgs[0].Content, "UNTRUSTED_DATA") {
		t.Fatalf("观察须以外部数据围栏写入: %+v", msgs)
	}
}
