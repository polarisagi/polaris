---
name: file_read
description: "Read and return the contents of a file within allowed directories."
version: "1.0.0"
tags:
  - filesystem
  - io
  - read
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# File Read

Read contents from a file on the local filesystem.

## Precondition
- File path must be within allowed directories
- File must exist and be readable

## Postcondition
- File content returned as string or bytes
