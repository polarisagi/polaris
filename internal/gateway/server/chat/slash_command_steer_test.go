package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
)

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestDispatch_Steer_NotConfigured(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	result := router.Dispatch(context.Background(), "/steer list", "s1", testHistory(), nil, rec, flusher, nil)
	if !result.Handled {
		t.Fatal("/steer 应被处理")
	}
	if !contains(result.Response, "未启用") {
		t.Errorf("expected not-enabled message, got %q", result.Response)
	}
}

func TestDispatch_Steer_ImportListDelete(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	cvStore := llmadapter.NewControlVectorStore()
	router.SetSteering(llmadapter.NewSteeringAdapter("http://unused.invalid/v1/steer", &http.Client{}), cvStore)

	// 无参数 → 用法提示
	res := router.Dispatch(context.Background(), "/steer", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "用法") {
		t.Errorf("expected usage message, got %q", res.Response)
	}

	// list 空
	res = router.Dispatch(context.Background(), "/steer list", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "尚无") {
		t.Errorf("expected empty-store message, got %q", res.Response)
	}

	// import 写临时文件后导入
	dir := t.TempDir()
	path := filepath.Join(dir, "cv.json")
	data, _ := json.Marshal(map[string]any{"layer": 12, "vector": []float32{0.1, 0.2}})
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	res = router.Dispatch(context.Background(), "/steer import polite "+path, "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "已导入") {
		t.Errorf("expected import success message, got %q", res.Response)
	}
	if cv, ok := cvStore.Get("polite"); !ok || cv.Layer != 12 || len(cv.Vector) != 2 {
		t.Fatalf("expected polite registered with layer=12 dim=2, got %+v ok=%v", cv, ok)
	}

	// list 非空
	res = router.Dispatch(context.Background(), "/steer list", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "polite") {
		t.Errorf("expected polite listed, got %q", res.Response)
	}

	// delete
	res = router.Dispatch(context.Background(), "/steer delete polite", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "已删除") {
		t.Errorf("expected delete success message, got %q", res.Response)
	}
	if _, ok := cvStore.Get("polite"); ok {
		t.Fatal("expected polite removed from store")
	}

	// delete 不存在的 label
	res = router.Dispatch(context.Background(), "/steer delete polite", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "未找到") {
		t.Errorf("expected not-found message, got %q", res.Response)
	}
}

func TestDispatch_Steer_SetAndDeactivate(t *testing.T) {
	var gotSteer, gotDelete bool
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			switch req.Method {
			case http.MethodPost:
				gotSteer = true
				b, _ := json.Marshal(llmadapter.SteerResponse{Applied: true, Layer: 12})
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(string(b))),
				}
			case http.MethodDelete:
				gotDelete = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
				}
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		}),
	}

	router, rec, flusher := newTestRouter(t)
	cvStore := llmadapter.NewControlVectorStore()
	cvStore.Import("polite", []float32{0.1, 0.2}, 12)
	router.SetSteering(llmadapter.NewSteeringAdapter("http://dummy", clientHTTP), cvStore)

	res := router.Dispatch(context.Background(), "/steer set polite 15", "s1", testHistory(), nil, rec, flusher, nil)
	if !gotSteer {
		t.Fatal("expected SteerActivations HTTP call to fire")
	}
	if !contains(res.Response, "已应用") {
		t.Errorf("expected apply success message, got %q", res.Response)
	}

	res = router.Dispatch(context.Background(), "/steer set unknown_label 15", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "未找到") {
		t.Errorf("expected not-found message for unknown label, got %q", res.Response)
	}

	res = router.Dispatch(context.Background(), "/steer deactivate", "s1", testHistory(), nil, rec, flusher, nil)
	if !gotDelete {
		t.Fatal("expected ClearSteering HTTP call to fire")
	}
	if !contains(res.Response, "已清除") {
		t.Errorf("expected clear success message, got %q", res.Response)
	}
}

func TestDispatch_Steer_CalibrateLayerNotSupported(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	router.SetSteering(llmadapter.NewSteeringAdapter("http://unused.invalid", &http.Client{}), llmadapter.NewControlVectorStore())

	res := router.Dispatch(context.Background(), "/steer calibrate-layer coding", "s1", testHistory(), nil, rec, flusher, nil)
	if !contains(res.Response, "暂未实现") {
		t.Errorf("expected not-implemented message, got %q", res.Response)
	}
}
