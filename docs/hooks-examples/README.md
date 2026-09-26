# Hooks 使用指南（hooks.json）

Polaris 采用 Claude / Codex 共同的 `hooks.json` 模型（ADR-0103 决策六）。

- 用户级：`~/.polarisagi/polaris/hooks/hooks.json`（视为已信任）
- 项目级：`<项目根>/.polaris/hooks/hooks.json`（需在「插件 › Hooks」审阅信任后才执行）
- 插件：插件包内 `hooks/hooks.json` 或清单 `hooks` 字段（同样需审阅信任；定义变更后须重新审阅）

## 示例：拦截危险命令、提交前提示

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "bash",
        "hooks": [
          { "type": "command", "if": "bash(rm *)", "command": "echo 'rm is not allowed' >&2; exit 2" }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          { "type": "prompt", "prompt": "Did the assistant run the tests before finishing? $ARGUMENTS" }
        ]
      }
    ]
  }
}
```

## 协议

- 输入 JSON 经 stdin 传入：`session_id`、`hook_event_name`、`tool_name`、`tool_input`、`prompt` 等。
- 退出码 0：成功，stdout 可输出 JSON 决策（`hookSpecificOutput.permissionDecision`、`updatedInput`、`additionalContext`、`decision: "block"`、`continue: false`）。
- 退出码 2：阻断，原因取 stderr。其他退出码：非阻断错误。
- 插件 hook 可用 `${CLAUDE_PLUGIN_ROOT}` / `${CLAUDE_PLUGIN_DATA}` 与环境变量 `CLAUDE_PLUGIN_OPTION_<KEY>`。
