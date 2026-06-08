// Hard constraints — compile-time enforced invariants
// Indivisible defaults. All forbid rules take precedence over permit.

forbid(principal, action, resource)
when {
    // Audit log is always on — can never be disabled
    resource has audit_log && resource.audit_log == false
};

forbid(principal, action, resource)
when {
    // Kill switch can never be bypassed
    action == Action::"bypass_killswitch"
};

forbid(principal, action, resource)
when {
    // Tainted data must never enter instruction context
    resource has tainted && resource.tainted == true &&
    context has zone && context.zone == "instruction"
};

// Privacy taint: PII data must not cross local-only boundary
forbid(principal, action, resource)
when {
    resource has pii && resource.pii == true &&
    action == Action::"transmit_external"
};
