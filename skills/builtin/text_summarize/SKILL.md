---
name: text_summarize
description: "Generate a concise summary of input text using the configured language model."
version: "1.0.0"
tags:
  - text
  - nlp
  - summarization
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Text Summarize

Generate a concise summary of input text using the configured reasoning model.

## Precondition
- Input text must be non-empty

## Postcondition
- Summary text returned
