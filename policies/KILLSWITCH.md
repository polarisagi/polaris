# KILLSWITCH — Emergency Stop Protocol

## Activation

Send `SIGUSR2` to the polaris process, or POST to `/_admin/kill`.

## Three-Stage Escalation

### Stage 1: THROTTLE
- Halve max concurrency
- Reject new agent sessions
- Allow in-flight tasks to complete (max 30s grace)
- Trigger: Token_Burn_Rate > 2x P95 sustained for 60s

### Stage 2: PAUSE
- Suspend all in-flight agent tasks
- Flush pending events to event log
- Hold open sessions (no new messages accepted)
- Trigger: Stage 1 not resolved within 60s OR free memory < 512MB

### Stage 3: FULL STOP
- Terminate all agent goroutines
- Write checkpoint to event log
- Close all sandboxes
- Exit process (exit code 1)
- Trigger: Manual admin command OR OSMemoryGuard critical threshold breached

## Recovery

Restart polaris. Sessions recover from event log replay (crash-durable, no checkpoint needed).

## Inviolable

This protocol CANNOT be bypassed by any agent or LLM output. Implemented as compile-time constants.
