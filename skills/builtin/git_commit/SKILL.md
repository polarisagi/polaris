---
name: git_commit
description: "Stage files and create a git commit with a message; returns the resulting commit hash."
version: "1.0.0"
tags:
  - git
  - vcs
  - commit
exec_mode: tool
risk_level: high
sandbox: L2
capability: write-local
---

# Git Commit

Stage and commit changes to a git repository.

## Precondition
- Working directory must be a git repository
- Commit message must be non-empty

## Postcondition
- Changes staged and committed
- Commit hash returned
