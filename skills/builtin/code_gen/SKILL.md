---
name: code_gen
description: "Generate code from a natural language specification and write it to the specified output file."
version: "1.0.0"
tags:
  - code
  - generation
  - ai
exec_mode: tool
risk_level: medium
sandbox: L2
capability: write-local
---

# Code Generate

Generate code from a natural language specification.

## Precondition
- Specification must be clear and unambiguous
- Output file path must be within project directories

## Postcondition
- Generated code written to specified file
- Passes syntax validation
