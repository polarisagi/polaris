# ESCALATE — Human-in-the-Loop Escalation Paths

## Trigger Conditions

| Condition | Escalation Level | Action |
|-----------|-----------------|--------|
| Privileged tool call requested | Level 1 | Await human approval (timeout: 5 min) |
| Code deploy to production | Level 2 | Require dual approval |
| Network access to new domain | Level 2 | Require domain allowlist update + approval |
| File write outside project dirs | Level 1 | Await human approval |
| L3+ self-evolution patch | Level 3 | Require multi-signature + full regression + shadow deploy |
| Budget threshold exceeded | Level 1 | Notify + await confirmation |

## Approval Flow

1. System emits HITL checkpoint event to blackboard
2. Notification sent via configured channel (CLI prompt / HTTP callback)
3. Human approves/denies within timeout window
4. On timeout: deny (safe default)
5. Approval recorded in immutable audit trail

## Bypass (Emergency Only)

Physical console access with admin key can override. All bypasses logged with reason and identity.
