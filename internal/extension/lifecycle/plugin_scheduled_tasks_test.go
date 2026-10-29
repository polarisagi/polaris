package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/workflowadmin"
)

// TestPlugin_ScheduledTasksLifecycle 集成测试（T10 验收）：
// 安装 → 模板可见 → 从模板创建实例 → 卸载 → 实例停用且保留数据。
func TestPlugin_ScheduledTasksLifecycle(t *testing.T) {
	ctx := context.Background()
	extRepo := newTestExtRepo(t)
	reg := &recordingSkillRegistry{}
	conn := &recordingConnector{}
	inst := NewPluginInstaller(extRepo, conn, reg).WithPolicyGate(allowAllGate{})

	// 1. 准备包含伴生任务的 fake 插件目录
	pluginDir := filepath.Join(t.TempDir(), "crawler-plugin")
	manifestJSON := `{
		"name": "crawler",
		"version": "1.0.0",
		"description": "Crawler with companion scheduled tasks",
		"author": {"name": "Polaris"},
		"mcpServers": {
			"browser": {
				"command": "node",
				"args": ["server.js"]
			}
		},
		"extensions": {
			"ai.polarisagi": {
				"scheduledTasks": [
					{
						"key": "daily_news",
						"name": "每日早间资讯",
						"description": "每天早上汇总行业动态",
						"kind": "prompt",
						"schedule": "0 8 * * *",
						"prompt": "汇总科技新闻"
					},
					{
						"key": "harvest_pipeline",
						"name": "知乎数据采选管线",
						"description": "定时采集并提炼知乎热门",
						"kind": "pipeline",
						"schedule": "0 10 * * *",
						"pipeline": {
							"steps": [
								{
									"key": "fetch",
									"kind": "tool",
									"tool": "${MCP:browser:harvest}",
									"args": {"site": "zhihu", "max_items": 10}
								}
							]
						}
					}
				]
			}
		}
	}`
	writeTestFile(t, filepath.Join(pluginDir, "plugin.json"), manifestJSON)
	writeTestFile(t, filepath.Join(pluginDir, "server.js"), "// mcp server")

	// 2. 执行安装
	res, err := inst.Install(ctx, InstallReq{
		InstID:    "ext_crawler",
		Name:      "crawler",
		LocalPath: pluginDir,
		TrustTier: 2,
	})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	expectedPluginID := "pl_crawler"
	if res.RuntimeID != expectedPluginID {
		t.Fatalf("expected runtimeID %q, got %q", expectedPluginID, res.RuntimeID)
	}

	// 3. 验证数据库中持久化的清单快照包含 ScheduledTasks
	var rawManifest string
	if err := extRepo.db.QueryRowContext(ctx, "SELECT manifest FROM plugins WHERE id=?", expectedPluginID).Scan(&rawManifest); err != nil {
		t.Fatalf("query manifest for %s: %v", expectedPluginID, err)
	}
	var manifest pluginspec.Plugin
	if err := json.Unmarshal([]byte(rawManifest), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(manifest.ScheduledTasks) != 2 {
		t.Fatalf("expected 2 scheduled tasks in manifest, got %d", len(manifest.ScheduledTasks))
	}
	if manifest.ScheduledTasks[0].Key != "daily_news" || manifest.ScheduledTasks[1].Key != "harvest_pipeline" {
		t.Fatalf("unexpected tasks in manifest: %+v", manifest.ScheduledTasks)
	}

	// 4. 验证插件目录内已写入 automation_templates.yaml 模板文件
	tplFile := filepath.Join(pluginDir, "automation_templates.yaml")
	if _, err := os.Stat(tplFile); os.IsNotExist(err) {
		t.Fatalf("expected automation_templates.yaml to exist in plugin dir")
	}

	// 5. 验证 CronAdmin 模板发现接口可见该模板且 source 为 plugin:pl_crawler
	ca := &cronadmin.CronAdmin{
		ExtRepo:          extRepo,
		TemplateCacheMap: &sync.Map{},
	}

	reqURL := "/v1/automation-templates?source=plugin:" + expectedPluginID
	httpReq := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()
	ca.HandleListAutomationTemplates(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleListAutomationTemplates returned status %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Templates []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			Source       string          `json:"source"`
			Kind         string          `json:"kind"`
			CronSchedule string          `json:"cron_schedule"`
			Prompt       string          `json:"prompt"`
			Pipeline     json.RawMessage `json:"pipeline"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal templates response: %v", err)
	}

	if len(resp.Templates) != 2 {
		t.Fatalf("expected 2 templates for plugin source, got %d", len(resp.Templates))
	}

	for _, tpl := range resp.Templates {
		if tpl.Source != "plugin:"+expectedPluginID {
			t.Errorf("expected source 'plugin:%s', got %q", expectedPluginID, tpl.Source)
		}
	}

	// 6. 验证 ${MCP:browser:harvest} 占位符解析
	wfAdmin := &workflowadmin.WorkflowAdmin{DB: extRepo.db}
	spec := pipeline.Spec{
		Kind: "pipeline",
		Name: "知乎数据采选管线",
		Steps: []pipeline.StepSpec{
			{
				Key:  "fetch",
				Kind: "tool",
				Tool: "${MCP:browser:harvest}",
			},
		},
	}

	resolvedSpec := wfAdmin.ResolvePipelineMCPPlaceholders(ctx, spec, "plugin:"+expectedPluginID)
	expectedToolPrefix := "mcp__"
	if len(resolvedSpec.Steps) == 0 || resolvedSpec.Steps[0].Tool == "${MCP:browser:harvest}" {
		t.Fatalf("expected placeholder ${MCP:browser:harvest} to be resolved, got %q", resolvedSpec.Steps[0].Tool)
	}
	if resolvedSpec.Steps[0].Tool[:len(expectedToolPrefix)] != expectedToolPrefix {
		t.Fatalf("expected tool to start with %q, got %q", expectedToolPrefix, resolvedSpec.Steps[0].Tool)
	}

	// 7. 模拟用户启用（从模板创建自动化与工作流）
	sourceTag := "plugin:" + expectedPluginID
	_, err = extRepo.db.ExecContext(ctx, `
		INSERT INTO automations (id, name, prompt, trigger_type, cron_schedule, enabled, source, created_at, updated_at)
		VALUES ('auto_test_1', '用户启用的早报', '汇总科技新闻', 'cron', '0 8 * * *', 1, ?, '2026-10-14T00:00:00Z', '2026-10-14T00:00:00Z')`,
		sourceTag)
	if err != nil {
		t.Fatalf("insert automation: %v", err)
	}

	_, err = extRepo.db.ExecContext(ctx, `
		INSERT INTO workflows (id, type, name, description, trigger_type, cron_schedule, enabled, source, created_at, updated_at)
		VALUES ('wf_test_1', 'dag', '用户启用的管线', '管线描述', 'cron', '0 10 * * *', 1, ?, '2026-10-14T00:00:00Z', '2026-10-14T00:00:00Z')`,
		sourceTag)
	if err != nil {
		t.Fatalf("insert workflow: %v", err)
	}

	// 8. 执行插件卸载
	err = inst.Uninstall(ctx, UninstallReq{
		InstID:    "ext_crawler",
		RuntimeID: expectedPluginID,
	})
	if err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	// 9. 验证模板文件已从插件目录删除
	if _, err := os.Stat(tplFile); !os.IsNotExist(err) {
		t.Errorf("automation_templates.yaml should be removed on uninstall")
	}

	// 10. 验证模板在 CronAdmin 中不再可见
	w2 := httptest.NewRecorder()
	ca.HandleListAutomationTemplates(w2, httpReq)
	var resp2 struct {
		Templates []any `json:"templates"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if len(resp2.Templates) != 0 {
		t.Fatalf("expected 0 templates after uninstall, got %d", len(resp2.Templates))
	}

	// 11. 验证实例化的 automation 与 workflow 行 enabled=0，且 last_run_error 标注「来源插件已卸载」，用户数据未丢失
	var autoEnabled int
	var autoError, autoPrompt string
	err = extRepo.db.QueryRowContext(ctx, `SELECT enabled, last_run_error, prompt FROM automations WHERE id='auto_test_1'`).Scan(&autoEnabled, &autoError, &autoPrompt)
	if err != nil {
		t.Fatalf("query automation: %v", err)
	}
	if autoEnabled != 0 {
		t.Errorf("expected automation enabled=0, got %d", autoEnabled)
	}
	if autoError != "来源插件已卸载" {
		t.Errorf("expected last_run_error='来源插件已卸载', got %q", autoError)
	}
	if autoPrompt != "汇总科技新闻" {
		t.Errorf("expected prompt preserved, got %q", autoPrompt)
	}

	var wfEnabled int
	var wfError, wfName string
	err = extRepo.db.QueryRowContext(ctx, `SELECT enabled, last_run_error, name FROM workflows WHERE id='wf_test_1'`).Scan(&wfEnabled, &wfError, &wfName)
	if err != nil {
		t.Fatalf("query workflow: %v", err)
	}
	if wfEnabled != 0 {
		t.Errorf("expected workflow enabled=0, got %d", wfEnabled)
	}
	if wfError != "来源插件已卸载" {
		t.Errorf("expected last_run_error='来源插件已卸载', got %q", wfError)
	}
	if wfName != "用户启用的管线" {
		t.Errorf("expected name preserved, got %q", wfName)
	}
}
