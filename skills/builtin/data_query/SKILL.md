---
name: data_query
description: "Run a read-only SQL SELECT query against a SQLite database and return result rows."
version: "1.0.0"
tags:
  - database
  - sql
  - query
exec_mode: tool
risk_level: low
sandbox: L1
capability: read-only
---

# Data Query

Query structured data sources (SQLite) using SQL.

## Precondition
- Query must be SELECT-only (read access)
- Database must be in allowed sources

## Postcondition
- Query results returned as structured rows
