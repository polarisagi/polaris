---
name: git_diff
description: "Show changes between git commits, branches, or the working tree as a unified diff."
version: "1.0.0"
tags:
  - git
  - vcs
  - diff
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Git Diff

Show changes between commits, branches, or the working tree.

## Precondition
- Working directory must be a git repository

## Postcondition
- Unified diff output returned
