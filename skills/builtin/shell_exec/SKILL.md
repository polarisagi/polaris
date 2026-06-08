---
name: shell_exec
description: "Execute a shell command in a sandboxed environment and return stdout, stderr, and exit code."
version: "1.0.0"
tags:
  - shell
  - exec
  - cli
exec_mode: tool
risk_level: high
sandbox: L2
capability: write-local
---

# Shell Exec

Execute a shell command in a Wasm-sandboxed environment.

## Precondition
- Command must be in allowed_commands list
- No network access unless explicitly granted

## Postcondition
- stdout/stderr returned
- Exit code recorded
