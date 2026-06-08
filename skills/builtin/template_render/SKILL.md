---
name: template_render
description: "Render a Go template string with provided data bindings and return the output."
version: "1.0.0"
tags:
  - template
  - text
  - rendering
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Template Render

Render a Go template string with provided data.

## Precondition
- Template must be valid Go template syntax

## Postcondition
- Rendered output returned
