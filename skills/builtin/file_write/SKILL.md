---
name: file_write
description: "Write or overwrite a file at a given path within allowed directories."
version: "1.0.0"
tags:
  - filesystem
  - io
  - write
exec_mode: tool
risk_level: medium
sandbox: L2
capability: write-local
---

# File Write

Write content to a file on the local filesystem.

## Precondition
- File path must be within allowed directories
- Overwrite requires explicit confirmation

## Postcondition
- File created or updated with new content
