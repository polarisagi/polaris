---
name: code_review
description: "Analyze code or a diff and return structured review feedback with severity, category, and suggestions."
version: "1.0.0"
tags:
  - code
  - review
  - analysis
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Code Review

Analyze a code diff and produce structured review feedback.

## Precondition
- Code content or diff must be provided

## Postcondition
- Review report with severity, category, suggestion, and line reference
