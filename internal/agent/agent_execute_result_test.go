package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type recordingOffloader struct {
	taskID  string
	content []byte
	err     error
}

func (o *recordingOffloader) Offload(_ context.Context, taskID string, content []byte) (string, error) {
	if o.err != nil {
		return "", o.err
	}
	o.taskID, o.content = taskID, content
	return "ref-1", nil
}

// 超过观察上限的结果须完整卸载，两种投影都在首行给出可执行的 read_tool_ref 提示，
// 并保留首尾（错误汇总多在末尾）。
func TestSpillExecResult_OffloadsAndRendersRetrievableRef(t *testing.T) {
	a := NewAgentWithDefaults("sess-spill")
	off := &recordingOffloader{}
	a.InjectToolRefOffloader(off)

	raw := []byte("BEGIN" + strings.Repeat("x", 20_000) + "FAILED: 3 tests")
	v := a.spillExecResult(context.Background(), raw)

	if string(off.content) != string(raw) {
		t.Fatalf("offloader must receive full output, got %d bytes", len(off.content))
	}
	for _, limit := range []int{maxExecResultBytes, fsm.ObservationMaxBytes} {
		out := string(v.render(limit))
		if len(out) > limit {
			t.Fatalf("render(%d) produced %d bytes", limit, len(out))
		}
		wantRef := `read_tool_ref(task_id="sess-spill", id="ref-1")`
		firstLine, _, _ := strings.Cut(out, "\n")
		if !strings.Contains(firstLine, wantRef) {
			t.Fatalf("first line must carry retrieval hint, got %q", firstLine)
		}
		if !strings.Contains(out, "BEGIN") || !strings.HasSuffix(out, "FAILED: 3 tests") {
			t.Fatalf("preview must keep head and tail")
		}
	}
}

func TestSpillExecResult_SmallResultInline(t *testing.T) {
	a := NewAgentWithDefaults("sess-spill-small")
	off := &recordingOffloader{}
	a.InjectToolRefOffloader(off)

	raw := []byte(`{"ok":true}`)
	v := a.spillExecResult(context.Background(), raw)
	if off.content != nil {
		t.Fatal("small result must not be offloaded")
	}
	if string(v.render(maxExecResultBytes)) != string(raw) {
		t.Fatal("small result must render unchanged")
	}
}

// 卸载失败或未注入卸载器时不得给出取不回的引用。
func TestSpillExecResult_OffloadFailureDeclaresNotRetained(t *testing.T) {
	for name, off := range map[string]*recordingOffloader{
		"offload error": {err: apperr.New(apperr.CodeResourceExhausted, "quota")},
		"no offloader":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			a := NewAgentWithDefaults("sess-spill-fail")
			if off != nil {
				a.InjectToolRefOffloader(off)
			}
			out := string(a.spillExecResult(context.Background(), []byte(strings.Repeat("y", 20_000))).render(maxExecResultBytes))
			if strings.Contains(out, "read_tool_ref") || !strings.Contains(out, "full output not retained") {
				t.Fatalf("expected not-retained notice, got first line %q", strings.SplitN(out, "\n", 2)[0])
			}
		})
	}
}
