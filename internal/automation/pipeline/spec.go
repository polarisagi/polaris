package pipeline

import "encoding/json"

// Spec 工作流管线规格定义（ADR-0114）。
type Spec struct {
	Kind         string     `json:"kind"`                    // 固定 "pipeline"
	Name         string     `json:"name"`                    // 必填，1..60 字符
	Description  string     `json:"description,omitempty"`  // 可选描述
	Trigger      Trigger    `json:"trigger"`                 // 触发器配置
	ResultAction string     `json:"result_action,omitempty"` // 默认 "session"
	DeliverFrom  string     `json:"deliver_from,omitempty"`  // 投递步骤 key
	Steps        []StepSpec `json:"steps"`                   // 步骤列表，1..20
}

// Trigger 管线触发器定义。
type Trigger struct {
	Type string `json:"type"`           // "cron" | "manual"
	Cron string `json:"cron,omitempty"` // 当 Type="cron" 时必填
}

// StepSpec 管线步骤规格。
type StepSpec struct {
	Key         string   `json:"key"`                    // 步骤唯一引用标识，^[a-z][a-z0-9_]{0,31}$
	Kind        string   `json:"kind"`                   // "tool" | "transform" | "llm" | "agent"
	Needs       []string `json:"needs,omitempty"`         // 依赖的前置步骤 key 列表
	SkipIfEmpty bool     `json:"skip_if_empty,omitempty"` // 输出 count==0 时是否标记并传播 skipped
	MaxRetries  int      `json:"max_retries,omitempty"`  // 步骤失败重试次数

	// tool 步骤参数
	Tool      string          `json:"tool,omitempty"`       // 工具名称
	Args      json.RawMessage `json:"args,omitempty"`       // 工具入参 JSON
	ItemsPath string          `json:"items_path,omitempty"` // 提取 items 数组的点路径
	Approval  string          `json:"approval,omitempty"`   // "" | "hitl"

	// transform 步骤参数
	Ops []Op `json:"ops,omitempty"` // 算子变换序列

	// llm 步骤参数
	Input           string          `json:"input,omitempty"`             // 上游输入步骤 key
	Fields          []string        `json:"fields,omitempty"`            // 选取的条目字段列表
	Prompt          string          `json:"prompt,omitempty"`            // 提示词
	Output          string          `json:"output,omitempty"`            // "markdown" | "json"
	JSONSchema      json.RawMessage `json:"json_schema,omitempty"`       // output="json" 时的 Schema
	MaxInputChars   int             `json:"max_input_chars,omitempty"`   // 最大输入字符数
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"` // 最大输出 Token 数
	Thinking        string          `json:"thinking,omitempty"`          // "off" | "low"

	// agent 步骤参数
	ReasoningEffort string `json:"reasoning_effort,omitempty"` // "low" | "medium" | "high" | "ultra"
}

// Op transform 算子定义。
type Op struct {
	Op          string            `json:"op"`                     // "concat"|"dedup"|"filter"|"sort"|"limit"|"pick"|"rename"|"truncate"
	From        []string          `json:"from,omitempty"`         // concat: 上游步骤 key 列表
	By          string            `json:"by,omitempty"`           // dedup / sort: 目标字段点路径
	Scope       string            `json:"scope,omitempty"`        // dedup: "run" | "workflow"
	Field       string            `json:"field,omitempty"`        // filter / truncate: 目标字段点路径
	Cmp         string            `json:"cmp,omitempty"`          // filter: "=="|"!="|">"|">="|"<"|"<="|"contains"|"not_contains"|"regex"
	Value       any               `json:"value,omitempty"`        // filter 比较值
	KeepMissing bool              `json:"keep_missing,omitempty"` // filter: 字段缺失时是否保留
	Desc        bool              `json:"desc,omitempty"`         // sort: 是否降序
	N           int               `json:"n,omitempty"`            // limit 数量
	Fields      []string          `json:"fields,omitempty"`       // pick 保留字段点路径列表
	Map         map[string]string `json:"map,omitempty"`          // rename 映射字典 {old: new}
	MaxChars    int               `json:"max_chars,omitempty"`    // truncate 截断字符数（rune）
}

// StepData 步骤执行归一化数据模型（State-in-DB）。
type StepData struct {
	Items     []map[string]any `json:"items"`               // 归一化条目数组
	Text      string           `json:"text,omitempty"`      // 纯文本输出或大模型回答
	Count     int              `json:"count"`               // items 长度
	Truncated bool             `json:"truncated,omitempty"` // 是否因超出单步字节预算被截断
}
