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
    action == Action::"tool_call" &&
    principal has capability_token &&
    resource.capability_level == principal.capability_token.level
};

// Default deny for tool calls exceeding capability
forbid(principal, action, resource)
when {
    action == Action::"tool_call" &&
    resource.capability_level == "privileged" &&
    principal.capability_token.level != "privileged"
};

// Allow users to manage plugins (e.g. toggle MCP)
permit(principal, action, resource)
when {
    principal has role && principal.role == "user" &&
    action == Action::"plugin:manage"
};

// Allow all Agent/User to perform standard execution actions
permit (
    principal in [Role::"user", Role::"agent"],
    action in [
        Action::"read_file",
        Action::"execute_code", // legacy support
        Action::"tool_execute",
        Action::"hook_execute",
        Action::"script_execute", // CodeAct and Skill
        Action::"process_spawn",  // MCP and Shell
        Action::"computer_use"
    ],
    resource
);
