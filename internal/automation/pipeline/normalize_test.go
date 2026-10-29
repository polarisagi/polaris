package pipeline_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
)

func TestNormalize_TopLevelArray(t *testing.T) {
	raw := []byte(`[{"id": 1, "title": "A"}, {"id": 2, "title": "B"}]`)
	data := pipeline.Normalize(raw, "", 10000)

	if data.Count != 2 || len(data.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", data.Count)
	}
	if data.Items[0]["title"] != "A" || data.Items[1]["title"] != "B" {
		t.Errorf("unexpected items content: %+v", data.Items)
	}
	if data.Truncated {
		t.Errorf("unexpected truncated flag")
	}
}

func TestNormalize_AutoDetectKeys(t *testing.T) {
	candidates := []struct {
		name string
		json string
	}{
		{"items", `{"items": [{"title": "Item 1"}], "total": 1}`},
		{"results", `{"results": [{"title": "Item 1"}], "page": 1}`},
		{"entries", `{"entries": [{"title": "Item 1"}], "feed": "test"}`},
		{"data", `{"data": [{"title": "Item 1"}], "status": "ok"}`},
	}

	for _, tc := range candidates {
		data := pipeline.Normalize([]byte(tc.json), "", 10000)
		if data.Count != 1 || len(data.Items) != 1 {
			t.Errorf("[%s] expected 1 item, got %d", tc.name, data.Count)
		} else if data.Items[0]["title"] != "Item 1" {
			t.Errorf("[%s] unexpected item content: %+v", tc.name, data.Items[0])
		}
	}
}

func TestNormalize_ItemsPath(t *testing.T) {
	raw := []byte(`{"response": {"nested": {"articles": [{"headline": "News"}]}}}`)
	data := pipeline.Normalize(raw, "response.nested.articles", 10000)

	if data.Count != 1 || len(data.Items) != 1 {
		t.Fatalf("expected 1 item with items_path, got %d", data.Count)
	}
	if data.Items[0]["headline"] != "News" {
		t.Errorf("unexpected item: %+v", data.Items[0])
	}
}

func TestNormalize_PlainTextAndNonArray(t *testing.T) {
	// 纯文本
	plain := []byte("plain text content")
	data := pipeline.Normalize(plain, "", 10000)
	if data.Count != 0 || len(data.Items) != 0 || data.Text != "plain text content" {
		t.Errorf("unexpected plain text normalize: %+v", data)
	}

	// 无数组的 JSON 对象
	obj := []byte(`{"status": "success", "message": "all done"}`)
	dataObj := pipeline.Normalize(obj, "", 10000)
	if dataObj.Count != 0 || len(dataObj.Items) != 0 || dataObj.Text != string(obj) {
		t.Errorf("unexpected json obj normalize: %+v", dataObj)
	}
}

func TestNormalize_Truncation(t *testing.T) {
	// 构造超大 items
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{
			"id":      i,
			"content": strings.Repeat("x", 200),
		}
	}
	raw, _ := json.Marshal(items)

	// 限制 1000 字节
	data := pipeline.Normalize(raw, "", 1000)
	if !data.Truncated {
		t.Errorf("expected Truncated=true")
	}
	if data.Count >= 100 || data.Count == 0 {
		t.Errorf("expected items to be pruned to fit 1000 bytes, got count=%d", data.Count)
	}
	marshaled, _ := json.Marshal(data)
	if len(marshaled) > 1000 {
		t.Errorf("expected marshaled size <= 1000, got %d", len(marshaled))
	}

	// 原始数字/字符串数组包装为 map[string]any{"value": item}
	primRaw := []byte(`["str1", "str2"]`)
	primData := pipeline.Normalize(primRaw, "", 10000)
	if primData.Count != 2 || primData.Items[0]["value"] != "str1" {
		t.Errorf("expected primitive values wrapped as value, got %+v", primData.Items)
	}

	// 路径未匹配
	missData := pipeline.Normalize([]byte(`{"other": 123}`), "nonexistent.path", 10000)
	if missData.Count != 0 {
		t.Errorf("expected 0 items for nonexistent items_path")
	}
}
