---
name: regex_match
description: "Match text against a regular expression pattern and return all matches with capture groups."
version: "1.0.0"
tags:
  - text
  - regex
  - matching
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Regex Match

Match text against a regular expression pattern.

## Precondition
- Pattern must be valid regex

## Postcondition
- Matches with groups returned
