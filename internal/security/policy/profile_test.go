package policy_test

import (
	"testing"

	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestProfilePolicy_Standard(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileStandard)
	if p.ForceContainerSandbox {
		t.Error("standard: ForceContainerSandbox should be false")
	}
	if !p.TaintEgressBlockEnabled {
		t.Error("standard: TaintEgressBlockEnabled should be true")
	}
	if !p.PIITokenizeEnabled {
		t.Error("standard: PIITokenizeEnabled should be true")
	}
	if p.CheckpointEnabled {
		t.Error("standard: CheckpointEnabled should be false")
	}
	if p.SecretGetEnabled {
		t.Error("standard: SecretGetEnabled should be false")
	}
	if p.SafeDialerAuditOnly {
		t.Error("standard: SafeDialerAuditOnly should be false")
	}
	if p.RunawayGuardDefaultRounds != 200 {
		t.Errorf("standard: RunawayGuardDefaultRounds should be 200, got %d", p.RunawayGuardDefaultRounds)
	}
	if p.SubagentMaxConcurrent != 6 {
		t.Errorf("standard: SubagentMaxConcurrent should be 6, got %d", p.SubagentMaxConcurrent)
	}
	if p.DelegationChainMaxDepth != 3 {
		t.Errorf("standard: DelegationChainMaxDepth should be 3, got %d", p.DelegationChainMaxDepth)
	}
	if !p.CodeActTaintBlockEnabled {
		t.Error("standard: CodeActTaintBlockEnabled should be true")
	}
	if !p.JobsKillOnSessionEnd {
		t.Error("standard: JobsKillOnSessionEnd should be true")
	}
	if p.SleepTimeEnabled {
		t.Error("standard: SleepTimeEnabled should be false")
	}
	if p.SleepTimeIdleMinutes != 10 {
		t.Errorf("standard: SleepTimeIdleMinutes should be 10, got %d", p.SleepTimeIdleMinutes)
	}
	if !p.BackgroundJobsAllowed {
		t.Error("standard: BackgroundJobsAllowed should be true")
	}
	if !p.PersistentTerminalAllowed {
		t.Error("standard: PersistentTerminalAllowed should be true")
	}
}

func TestProfilePolicy_Full(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileFull)
	if p.ForceContainerSandbox {
		t.Error("full: ForceContainerSandbox must be false")
	}
	if p.TaintEgressBlockEnabled {
		t.Error("full: TaintEgressBlockEnabled should be false (audit-only)")
	}
	if p.PIITokenizeEnabled {
		t.Error("full: PIITokenizeEnabled should be false")
	}
	if !p.CheckpointEnabled {
		t.Error("full: CheckpointEnabled should be true")
	}
	if !p.SecretGetEnabled {
		t.Error("full: SecretGetEnabled should be true")
	}
	if !p.SafeDialerAuditOnly {
		t.Error("full: SafeDialerAuditOnly should be true")
	}
	if p.RunawayGuardDefaultRounds != 0 {
		t.Errorf("full: RunawayGuardDefaultRounds should be 0, got %d", p.RunawayGuardDefaultRounds)
	}
	if p.SubagentMaxConcurrent != 16 {
		t.Errorf("full: SubagentMaxConcurrent should be 16, got %d", p.SubagentMaxConcurrent)
	}
	if p.DelegationChainMaxDepth != 8 {
		t.Errorf("full: DelegationChainMaxDepth should be 8, got %d", p.DelegationChainMaxDepth)
	}
	if p.CodeActTaintBlockEnabled {
		t.Error("full: CodeActTaintBlockEnabled should be false")
	}
	if p.JobsKillOnSessionEnd {
		t.Error("full: JobsKillOnSessionEnd should be false")
	}
	if !p.SleepTimeEnabled {
		t.Error("full: SleepTimeEnabled should be true")
	}
	if p.SleepTimeIdleMinutes != 10 {
		t.Errorf("full: SleepTimeIdleMinutes should be 10, got %d", p.SleepTimeIdleMinutes)
	}
	if !p.BackgroundJobsAllowed {
		t.Error("full: BackgroundJobsAllowed should be true")
	}
	if !p.PersistentTerminalAllowed {
		t.Error("full: PersistentTerminalAllowed should be true")
	}
}

func TestProfilePolicy_Hardened(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileHardened)
	if !p.ForceContainerSandbox {
		t.Error("hardened: ForceContainerSandbox should be true")
	}
	if !p.TaintEgressBlockEnabled {
		t.Error("hardened: TaintEgressBlockEnabled should be true")
	}
	if !p.PIITokenizeEnabled {
		t.Error("hardened: PIITokenizeEnabled should be true")
	}
	if p.CheckpointEnabled {
		t.Error("hardened: CheckpointEnabled should be false")
	}
	if p.SecretGetEnabled {
		t.Error("hardened: SecretGetEnabled should be false")
	}
	if p.SafeDialerAuditOnly {
		t.Error("hardened: SafeDialerAuditOnly should be false")
	}
	if p.RunawayGuardDefaultRounds != 50 {
		t.Errorf("hardened: RunawayGuardDefaultRounds should be 50, got %d", p.RunawayGuardDefaultRounds)
	}
	if p.SubagentMaxConcurrent != 2 {
		t.Errorf("hardened: SubagentMaxConcurrent should be 2, got %d", p.SubagentMaxConcurrent)
	}
	if p.DelegationChainMaxDepth != 2 {
		t.Errorf("hardened: DelegationChainMaxDepth should be 2, got %d", p.DelegationChainMaxDepth)
	}
	if !p.CodeActTaintBlockEnabled {
		t.Error("hardened: CodeActTaintBlockEnabled should be true")
	}
	if !p.JobsKillOnSessionEnd {
		t.Error("hardened: JobsKillOnSessionEnd should be true")
	}
	if p.SleepTimeEnabled {
		t.Error("hardened: SleepTimeEnabled should be false")
	}
	if p.SleepTimeIdleMinutes != 10 {
		t.Errorf("hardened: SleepTimeIdleMinutes should be 10, got %d", p.SleepTimeIdleMinutes)
	}
	if p.BackgroundJobsAllowed {
		t.Error("hardened: BackgroundJobsAllowed should be false")
	}
	if p.PersistentTerminalAllowed {
		t.Error("hardened: PersistentTerminalAllowed should be false")
	}
}

// 确认空字符串归一化为 standard，行为与显式 standard 完全等价。
func TestProfilePolicy_EmptyNormalizesToStandard(t *testing.T) {
	empty := policy.ProfilePolicy("")
	standard := policy.ProfilePolicy(policy.ProfileStandard)
	if empty != standard {
		t.Errorf("empty profile should equal standard: got %+v, want %+v", empty, standard)
	}
}

// 确认未知档位也归一化为 standard（防御未来配置错误）。
func TestProfilePolicy_UnknownNormalizesToStandard(t *testing.T) {
	unknown := policy.ProfilePolicy("unknown_future_profile")
	standard := policy.ProfilePolicy(policy.ProfileStandard)
	if unknown != standard {
		t.Errorf("unknown profile should equal standard: got %+v, want %+v", unknown, standard)
	}
}

// MaxPermissionMode 与 CapMode：hardened 把偏好压到 auto_review，其余不设限（ADR-0121 决策一）。
func TestProfilePolicy_CapMode(t *testing.T) {
	hard := policy.ProfilePolicy(policy.ProfileHardened)
	std := policy.ProfilePolicy(policy.ProfileStandard)
	full := policy.ProfilePolicy(policy.ProfileFull)
	if hard.MaxPermissionMode != types.ModeAutoReview {
		t.Errorf("hardened max = %q", hard.MaxPermissionMode)
	}
	if std.MaxPermissionMode != types.ModeFullAccess || full.MaxPermissionMode != types.ModeFullAccess {
		t.Error("standard/full max should be full_access")
	}
	cases := []struct {
		p       policy.Policy
		in, out types.PermissionMode
	}{
		{hard, types.ModeFullAccess, types.ModeAutoReview},
		{hard, types.ModeAutoReview, types.ModeAutoReview},
		{hard, types.ModeDefault, types.ModeDefault},
		{std, types.ModeFullAccess, types.ModeFullAccess},
		{full, types.ModeDefault, types.ModeDefault},
		{policy.Policy{}, types.ModeFullAccess, types.ModeFullAccess},
	}
	for _, c := range cases {
		if got := c.p.CapMode(c.in); got != c.out {
			t.Errorf("%s CapMode(%s) = %s, want %s", c.p.Profile, c.in, got, c.out)
		}
	}
}
