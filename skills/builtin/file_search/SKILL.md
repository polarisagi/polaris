---
name: file_search
description: "Recursively search file contents by pattern and return matched lines with file paths and line numbers."
version: "1.0.0"
tags:
  - filesystem
  - search
  - grep
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# File Search

Search file contents recursively using pattern matching.

## Precondition
- Root path must be within allowed directories

## Postcondition
- List of matched files with line numbers returned
