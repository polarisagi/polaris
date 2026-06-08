---
name: text_extract
description: "Extract structured entities, key-value pairs, or tables from unstructured text."
version: "1.0.0"
tags:
  - text
  - nlp
  - extraction
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Text Extract

Extract structured data (entities, key-value pairs, tables) from unstructured text.

## Precondition
- Input text must be non-empty
- Extraction schema must be specified

## Postcondition
- Structured data returned matching the specified schema
