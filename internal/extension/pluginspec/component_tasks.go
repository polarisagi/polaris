package pluginspec

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	RuleScheduledTasksShape = "polaris.scheduledTasks.shape"
	RuleScheduledTasksKey   = "polaris.scheduledTasks.key"
	RuleScheduledTasksCron  = "polaris.scheduledTasks.schedule"
	polarisNamespaceKey     = "ai.polarisagi"
)

var scheduledTaskKeyPattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
var mcpPlaceholderPattern = regexp.MustCompile(`\$\{MCP:([a-zA-Z0-9_-]+):([a-zA-Z0-9_-]+)\}`)

// ScheduledTask 插件伴生自动化/管线任务（ADR-0114 / ADR-0103：定义在 extensions["ai.polarisagi"].scheduledTasks）。
// 安装时不直接生成运行实例，而是注册为自动化模板供用户选择启用（安装 ≠ 启用）。
type ScheduledTask struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Kind        string          `json:"kind"` // "prompt" | "pipeline"
	Schedule    string          `json:"schedule"`
	Prompt      string          `json:"prompt,omitempty"`
	Pipeline    json.RawMessage `json:"pipeline,omitempty"`
}

type scheduledTaskWire struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Schedule    string          `json:"schedule"`
	Prompt      string          `json:"prompt"`
	Pipeline    json.RawMessage `json:"pipeline"`
}

// loadScheduledTasks 从清单文档中提取 extensions["ai.polarisagi"].scheduledTasks。
// 遵循严格对象规则：未知键或格式非法仅导致单项失败并记入诊断，不影响插件其余组件的加载。
func loadScheduledTasks(docs []manifestDoc, ds *diagnostics) []ScheduledTask {
	var out []ScheduledTask
	seenKeys := make(map[string]bool)

	for _, doc := range docs {
		rawExt, ok := doc.wire.Extensions[polarisNamespaceKey]
		if !ok || len(rawExt) == 0 || string(rawExt) == "null" {
			continue
		}

		var polarisExt struct {
			ScheduledTasks []json.RawMessage `json:"scheduledTasks"`
		}
		if err := json.Unmarshal(rawExt, &polarisExt); err != nil {
			ds.errorf("scheduled_task", doc.path, RuleScheduledTasksShape,
				"extensions[%q] must be a JSON object: %v", polarisNamespaceKey, err)
			continue
		}

		for _, item := range polarisExt.ScheduledTasks {
			task, ok := parseScheduledTaskItem(item, doc.path, ds)
			if !ok {
				continue
			}
			if seenKeys[task.Key] {
				ds.warnf("scheduled_task", doc.path, RuleScheduledTasksKey,
					"duplicate scheduled task key %q skipped", task.Key)
				continue
			}
			seenKeys[task.Key] = true
			out = append(out, *task)
		}
	}

	return out
}

func parseScheduledTaskItem(item json.RawMessage, path string, ds *diagnostics) (*ScheduledTask, bool) {
	var w scheduledTaskWire
	dec := json.NewDecoder(bytes.NewReader(item))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
			"scheduled task entry invalid (unknown fields or syntax error): %v", err)
		return nil, false
	}
	if !validateScheduledTaskBasic(&w, path, ds) || !validateScheduledTaskKind(&w, path, ds) {
		return nil, false
	}
	return &ScheduledTask{
		Key:         w.Key,
		Name:        strings.TrimSpace(w.Name),
		Description: strings.TrimSpace(w.Description),
		Kind:        w.Kind,
		Schedule:    strings.TrimSpace(w.Schedule),
		Prompt:      w.Prompt,
		Pipeline:    w.Pipeline,
	}, true
}

func validateScheduledTaskBasic(w *scheduledTaskWire, path string, ds *diagnostics) bool {
	if !scheduledTaskKeyPattern.MatchString(w.Key) {
		ds.errorf("scheduled_task", path, RuleScheduledTasksKey,
			"scheduled task key %q must match ^[a-z0-9_]{1,40}$", w.Key)
		return false
	}
	if strings.TrimSpace(w.Name) == "" {
		ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
			"scheduled task %q: name is required", w.Key)
		return false
	}
	fields := strings.Fields(strings.TrimSpace(w.Schedule))
	if len(fields) != 5 {
		ds.errorf("scheduled_task", path, RuleScheduledTasksCron,
			"scheduled task %q: schedule %q must be a 5-field cron expression", w.Key, w.Schedule)
		return false
	}
	return true
}

func validateScheduledTaskKind(w *scheduledTaskWire, path string, ds *diagnostics) bool {
	if w.Kind != "prompt" && w.Kind != "pipeline" {
		ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
			"scheduled task %q: kind must be prompt or pipeline, got %q", w.Key, w.Kind)
		return false
	}
	if w.Kind == "prompt" && strings.TrimSpace(w.Prompt) == "" {
		ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
			"scheduled task %q: prompt is required for kind=prompt", w.Key)
		return false
	}
	if w.Kind == "pipeline" {
		if len(w.Pipeline) == 0 || string(w.Pipeline) == "null" {
			ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
				"scheduled task %q: pipeline spec is required for kind=pipeline", w.Key)
			return false
		}
		var obj map[string]any
		if err := json.Unmarshal(w.Pipeline, &obj); err != nil {
			ds.errorf("scheduled_task", path, RuleScheduledTasksShape,
				"scheduled task %q: pipeline spec must be a valid JSON object: %v", w.Key, err)
			return false
		}
	}
	return true
}

// ResolveMCPPlaceholders 解析并替换文本中声明的 ${MCP:<server>:<tool>} 占位符。
// resolveServer 接收清单中声明的 server 名称，返回安装后的实际 scoped server 名称或完整工具名。
// 若 resolveServer 返回空字符串，保留原占位符不作替换，以便后续工具校验阶段捕获未就绪的依赖。
func ResolveMCPPlaceholders(input []byte, resolveServer func(server string) string) []byte {
	return mcpPlaceholderPattern.ReplaceAllFunc(input, func(match []byte) []byte {
		sub := mcpPlaceholderPattern.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		server := string(sub[1])
		tool := string(sub[2])
		var resolved string
		if resolveServer != nil {
			resolved = resolveServer(server)
		}
		if resolved == "" {
			return match
		}
		if strings.HasPrefix(resolved, "mcp__") {
			return []byte(resolved)
		}
		return []byte("mcp__" + resolved + "__" + tool)
	})
}

// ResolveMCPPlaceholdersString 为 ResolveMCPPlaceholders 的字符串版本。
func ResolveMCPPlaceholdersString(input string, resolveServer func(server string) string) string {
	return string(ResolveMCPPlaceholders([]byte(input), resolveServer))
}
