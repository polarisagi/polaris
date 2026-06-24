// Soft constraints — hot-reloadable policy rules
// Evaluated after hard constraints pass.

permit(principal, action, resource)
when {
    principal has role && principal.role == "user" &&
    resource has capability && resource.capability == "read"
};

permit(principal, action, resource)
when {
    principal has role && principal.role == "agent" &&
    resource in principal.allowed_resources
};

// Tool call requires matched capability token
permit(principal, action, resource)
when {
    action == Action::"tool_execute" &&
    principal has capability_token &&
    resource.capability_level == principal.capability_token.level
};

// Default deny for tool calls exceeding capability
forbid(principal, action, resource)
when {
    action == Action::"tool_execute" &&
    resource.capability_level == "privileged" &&
    principal.capability_token.level != "privileged"
};

// Allow users to manage plugins (e.g. toggle MCP)
permit(principal, action, resource)
when {
    principal has role && principal.role == "user" &&
    action == Action::"plugin:manage"
};

// Allow users/agents to perform standard legacy execution actions
permit (
    principal in [Role::"user", Role::"agent"],
    action in [
        Action::"read_file",
        Action::"execute_code" // legacy support
    ],
    resource
);

// MCP 进程生成：Official+ 放行；Community 需自动沙箱
permit (principal == Agent::"mcp_mgr", action == Action::"process_spawn", resource is MCPServer)
when { context.trust_tier >= 3 };
permit (principal == Agent::"mcp_mgr", action == Action::"process_spawn", resource is MCPServer)
when { context.trust_tier == 2 && context.sandbox_auto == true };

// Skill / CodeAct 脚本：Local+ 放行（隔离等级由 SandboxFloor 保证）
permit (principal == Agent::"agent", action == Action::"script_execute", resource is Script)
when { context.trust_tier >= 1 };

// CodeAct 是 TrustUntrusted(0)：需显式放行（隔离 L2 + govAgent 已是边界）
permit (principal == Agent::"agent", action == Action::"script_execute", resource is Script)
when { context.tool_source == "llm_generated" };

// Hook：用户配置，放行（低 TrustTier 走 L2/L3 隔离）
permit (principal == Agent::"agent", action == Action::"hook_execute", resource is HookEvent);

// LAM：必须 allow_net=true
permit (principal == Agent::"agent", action == Action::"browser_automate", resource == "lam")
when { context.allow_net == true };
