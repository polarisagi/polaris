// memory_write: Agent 自写记忆，默认 permit（LLM 生成内容 TaintLow）
permit (
    principal is Agent,
    action == Action::"tool_execute",
    resource == Tool::"memory_write"
)
when { resource.taint_level <= 2 };

// memory_search: 无副作用，unconditionally permit
permit (
    principal is Agent,
    action == Action::"tool_execute",
    resource == Tool::"memory_search"
);

// memory_expire: 允许 Agent 撤销自写内容
permit (
    principal is Agent,
    action == Action::"tool_execute",
    resource == Tool::"memory_expire"
)
when { resource.source_type == "user_stated" };

// memory_write with TaintHigh: forbid（防止高污点内容持久化到语义记忆）
forbid (
    principal is Agent,
    action == Action::"tool_execute",
    resource == Tool::"memory_write"
)
when { resource.taint_level >= 3 };
