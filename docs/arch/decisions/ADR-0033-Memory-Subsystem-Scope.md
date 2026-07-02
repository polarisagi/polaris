# ADR 0033: Memory Subsystem Scope Limits (G-4 Decisions)

## Status
Accepted

## Date
2026-07-02

## Context
During the comprehensive Memory & Learning Subsystem Upgrade (M05/M09 refactoring), several potential capabilities were identified for potential development. While some were successfully incorporated (such as the SurrealDB boot replayer and unified MemoryFacade interfaces), three specific architectural enhancements were actively evaluated but deliberately excluded from the current roadmap to prevent over-engineering, scope creep, and unnecessary complexity.

## Decisions

We explicitly decide **NOT** to implement the following items at this time (the "Won't Do" list):

1. **SurrealKV O(1) Signature Search (from M05 §5):**
   - *Proposal:* Dual-track skill retrieval that bypasses SQLite for faster O(1) lookup.
   - *Rationale:* The M6 architecture already features an SQLite-based Registry + Selector which performs adequately for the current scale. Adding a dual-track SurrealKV path would introduce state synchronization complexity without providing clear, immediate performance benefits for the expected skill load.

2. **Activation Steering (from M09 §1.3):**
   - *Proposal:* A local-only, hardware-gated capability for real-time model activation steering.
   - *Rationale:* This capability tightly couples the system to specific local hardware inference engines. At this stage, our focus remains on higher-level systemic learning and generalized memory consolidation.

3. **L3/L4 Evolution Multi-Signature Approval:**
   - *Proposal:* A mechanism requiring multi-party cryptographic signatures for high-tier autonomous self-evolution workflows.
   - *Rationale:* This represents an advanced security boundary design decision. The current heuristic-based autonomous improvements and sandbox fail-closed mechanics are sufficient. Introducing multi-signature requirements now would drastically slow down self-improvement velocity.

## Consequences
- The architecture remains leaner and the memory synchronization loops (Episodic -> Semantic) are not burdened by extraneous caching requirements.
- We rely primarily on standard SQLite FTS/BM25 and SurrealDB HNSW for skill/memory retrieval.
- We accept that local-only hardware-accelerated Activation Steering is deferred to a future iteration if model capabilities demand it.
