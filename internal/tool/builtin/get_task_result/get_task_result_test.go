package get_task_result

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeProvider 是 AsyncTaskProvider 的纯内存测试实现，不依赖 internal/extension/mcp，
// 用于独立验证 get_task_result 工具对 pending/done/failed/not-found 四种状态的处理。
type fakeProvider struct {
	status string
	text   string
	errMsg string
	images []types.ImagePart
	found  bool
}

func (f *fakeProvider) GetAsyncTaskResult(_ string) (status, text, errMsg string, images []types.ImagePart, found bool) {
	return f.status, f.text, f.errMsg, f.images, f.found
}

func TestGetTaskResultFn_NilProvider(t *testing.T) {
	fn := MakeGetTaskResultFn(nil)
	res, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{"task_id":"anything"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]string
	if uerr := json.Unmarshal(res.Output, &out); uerr != nil {
		t.Fatalf("failed to unmarshal output: %v", uerr)
	}
	if out["status"] != "expired_or_not_found" {
		t.Errorf("expected expired_or_not_found, got %q", out["status"])
	}
}

func TestGetTaskResultFn_MissingTaskID(t *testing.T) {
	fn := MakeGetTaskResultFn(nil)
	if _, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{}`)}); err == nil {
		t.Fatalf("expected error for missing task_id")
	}
}

func TestGetTaskResultFn_NotFound(t *testing.T) {
	fn := MakeGetTaskResultFn(&fakeProvider{found: false})
	res, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{"task_id":"no-such-task"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]string
	if uerr := json.Unmarshal(res.Output, &out); uerr != nil {
		t.Fatalf("failed to unmarshal output: %v", uerr)
	}
	if out["status"] != "expired_or_not_found" {
		t.Errorf("expected expired_or_not_found, got %q", out["status"])
	}
}

func TestGetTaskResultFn_Pending(t *testing.T) {
	fn := MakeGetTaskResultFn(&fakeProvider{found: true, status: "pending"})
	res, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{"task_id":"t1"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]string
	_ = json.Unmarshal(res.Output, &out)
	if out["status"] != "pending" {
		t.Errorf("expected pending, got %q", out["status"])
	}
}

func TestGetTaskResultFn_Done(t *testing.T) {
	fn := MakeGetTaskResultFn(&fakeProvider{found: true, status: "done", text: "hello world"})
	res, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{"task_id":"t1"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]string
	_ = json.Unmarshal(res.Output, &out)
	if out["status"] != "done" || out["result"] != "hello world" {
		t.Errorf("unexpected output: %+v", out)
	}
}

func TestGetTaskResultFn_Failed(t *testing.T) {
	fn := MakeGetTaskResultFn(&fakeProvider{found: true, status: "failed", errMsg: "boom"})
	res, err := fn(context.Background(), sandbox.SandboxSpec{Input: []byte(`{"task_id":"t1"}`)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out map[string]string
	_ = json.Unmarshal(res.Output, &out)
	if out["status"] != "failed" || out["error"] != "boom" {
		t.Errorf("unexpected output: %+v", out)
	}
}
