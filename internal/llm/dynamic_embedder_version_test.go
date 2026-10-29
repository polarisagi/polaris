package llm

import (
	"context"
	"sync"
	"testing"
)

type versionedStub struct{ v string }

func (s versionedStub) Embed(context.Context, string) []float32 { return []float32{1} }
func (s versionedStub) ModelVersion() string                    { return s.v }

type plainStub struct{}

func (plainStub) Embed(context.Context, string) []float32 { return []float32{1} }

// ADR-0109 P4：模型版本是重嵌与清库的唯一判据，必须随引擎切换而变化，且在回调执行时已是新值。
func TestDynamicEmbedder_ModelVersion(t *testing.T) {
	d := NewDynamicEmbedder()
	if d.ModelVersion() != "" {
		t.Fatal("unset embedder must report empty version")
	}
	var seen []string
	d.OnSet(func() { seen = append(seen, d.ModelVersion()) })

	d.Set(versionedStub{v: "onnx:a@512"})
	d.SetWithVersion(plainStub{}, "remote:m@1536")
	d.Set(plainStub{}) // 无 ModelVersion 且未显式给出 → ""

	want := []string{"onnx:a@512", "remote:m@1536", ""}
	if len(seen) != len(want) {
		t.Fatalf("seen=%v want=%v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen=%v want=%v", seen, want)
		}
	}
}

// 并发 Set 不得因重复 close(readyCh) panic。
func TestDynamicEmbedder_ConcurrentSet(t *testing.T) {
	d := NewDynamicEmbedder()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.Set(plainStub{}) }()
	}
	wg.Wait()
	<-d.WaitReady()
}
