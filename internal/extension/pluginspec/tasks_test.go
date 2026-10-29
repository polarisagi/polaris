package pluginspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScheduledTasks_Valid(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"name": "task-plugin",
		"version": "1.0.0",
		"description": "Test scheduled tasks",
		"extensions": {
			"ai.polarisagi": {
				"scheduledTasks": [
					{
						"key": "daily_brief",
						"name": "每日早报",
						"description": "每天早晨总结科技要闻",
						"kind": "prompt",
						"schedule": "0 8 * * *",
						"prompt": "总结今天的新闻"
					},
					{
						"key": "pipeline_harvest",
						"name": "数据管线",
						"description": "定时抓取与分析",
						"kind": "pipeline",
						"schedule": "*/30 * * * *",
						"pipeline": {
							"steps": [
								{
									"key": "fetch",
									"kind": "tool",
									"tool": "${MCP:browser:harvest}",
									"args": {"site": "zhihu"}
								}
							]
						}
					}
				]
			}
		}
	}`

	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p, err := Load(dir, LoadOptions{})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(p.ScheduledTasks) != 2 {
		t.Fatalf("expected 2 scheduled tasks, got %d", len(p.ScheduledTasks))
	}

	t0 := p.ScheduledTasks[0]
	if t0.Key != "daily_brief" || t0.Kind != "prompt" || t0.Schedule != "0 8 * * *" || t0.Prompt != "总结今天的新闻" {
		t.Errorf("task 0 mismatch: %+v", t0)
	}

	t1 := p.ScheduledTasks[1]
	if t1.Key != "pipeline_harvest" || t1.Kind != "pipeline" || t1.Schedule != "*/30 * * * *" || len(t1.Pipeline) == 0 {
		t.Errorf("task 1 mismatch: %+v", t1)
	}

	// 验证 Diagnostics 中无错误
	for _, d := range p.Diagnostics {
		if d.Severity == SeverityError {
			t.Errorf("unexpected error diagnostic: %+v", d)
		}
	}
}

func TestScheduledTasks_StrictObjectAndValidation(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"name": "strict-task-plugin",
		"version": "1.0.0",
		"extensions": {
			"ai.polarisagi": {
				"scheduledTasks": [
					{
						"key": "unknown_field_task",
						"name": "含未知字段的任务",
						"kind": "prompt",
						"schedule": "0 9 * * *",
						"prompt": "hello",
						"unknownKey": "foo"
					},
					{
						"key": "bad-key-with-dashes",
						"name": "非法 key",
						"kind": "prompt",
						"schedule": "0 9 * * *",
						"prompt": "hello"
					},
					{
						"key": "bad_cron",
						"name": "非法 cron",
						"kind": "prompt",
						"schedule": "* * *",
						"prompt": "hello"
					},
					{
						"key": "bad_kind",
						"name": "非法 kind",
						"kind": "unknown_kind",
						"schedule": "0 9 * * *",
						"prompt": "hello"
					},
					{
						"key": "missing_prompt",
						"name": "缺少 prompt",
						"kind": "prompt",
						"schedule": "0 9 * * *"
					},
					{
						"key": "missing_pipeline",
						"name": "缺少 pipeline",
						"kind": "pipeline",
						"schedule": "0 9 * * *"
					},
					{
						"key": "valid_task",
						"name": "合法任务",
						"kind": "prompt",
						"schedule": "0 12 * * *",
						"prompt": "valid prompt"
					}
				]
			}
		}
	}`

	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p, err := Load(dir, LoadOptions{})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// 只有 valid_task 应该保留
	if len(p.ScheduledTasks) != 1 {
		t.Fatalf("expected 1 valid task, got %d", len(p.ScheduledTasks))
	}
	if p.ScheduledTasks[0].Key != "valid_task" {
		t.Fatalf("expected valid_task, got %s", p.ScheduledTasks[0].Key)
	}

	// 必须收集全部对应错误规则的诊断
	ruleCount := make(map[string]int)
	for _, d := range p.Diagnostics {
		ruleCount[d.Rule]++
	}

	if ruleCount[RuleScheduledTasksShape] == 0 {
		t.Errorf("expected diagnostic for %s", RuleScheduledTasksShape)
	}
	if ruleCount[RuleScheduledTasksKey] == 0 {
		t.Errorf("expected diagnostic for %s", RuleScheduledTasksKey)
	}
	if ruleCount[RuleScheduledTasksCron] == 0 {
		t.Errorf("expected diagnostic for %s", RuleScheduledTasksCron)
	}
}

func TestResolveMCPPlaceholders(t *testing.T) {
	specJSON := []byte(`{
		"steps": [
			{"key": "s1", "tool": "${MCP:browser:harvest}", "url": "https://example.com"},
			{"key": "s2", "tool": "${MCP:db:query}", "args": {"limit": 10}},
			{"key": "s3", "tool": "${MCP:unresolved:cmd}"},
			{"key": "s4", "tool": "web_fetch"}
		]
	}`)

	resolved := ResolveMCPPlaceholders(specJSON, func(server string) string {
		switch server {
		case "browser":
			return "browser_use-browser"
		case "db":
			return "mcp__my_db__query_tool" // 直接返回完整工具名前缀
		default:
			return ""
		}
	})

	var parsed struct {
		Steps []struct {
			Key  string `json:"key"`
			Tool string `json:"tool"`
		} `json:"steps"`
	}

	if err := json.Unmarshal(resolved, &parsed); err != nil {
		t.Fatalf("Unmarshal resolved JSON failed: %v", err)
	}

	if len(parsed.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(parsed.Steps))
	}

	// s1 替换为 mcp__browser_use-browser__harvest
	if parsed.Steps[0].Tool != "mcp__browser_use-browser__harvest" {
		t.Errorf("s1 expected 'mcp__browser_use-browser__harvest', got %q", parsed.Steps[0].Tool)
	}

	// s2 替换为 mcp__my_db__query_tool
	if parsed.Steps[1].Tool != "mcp__my_db__query_tool" {
		t.Errorf("s2 expected 'mcp__my_db__query_tool', got %q", parsed.Steps[1].Tool)
	}

	// s3 未知 server，保持原样
	if parsed.Steps[2].Tool != "${MCP:unresolved:cmd}" {
		t.Errorf("s3 expected '${MCP:unresolved:cmd}', got %q", parsed.Steps[2].Tool)
	}

	// s4 普通工具，不受影响
	if parsed.Steps[3].Tool != "web_fetch" {
		t.Errorf("s4 expected 'web_fetch', got %q", parsed.Steps[3].Tool)
	}

	// 字符串辅助函数测试
	strRes := ResolveMCPPlaceholdersString("${MCP:browser:click}", func(s string) string {
		return "browser_use"
	})
	if strRes != "mcp__browser_use__click" {
		t.Errorf("ResolveMCPPlaceholdersString expected 'mcp__browser_use__click', got %q", strRes)
	}
}
