package policy_test

import (
	"testing"

	"github.com/polarisagi/polaris/internal/security/policy"
)

func TestProfilePolicy_Standard(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileStandard)
	if p.SandboxDisabled {
		t.Error("standard: SandboxDisabled should be false")
	}
	if p.ForceContainerSandbox {
		t.Error("standard: ForceContainerSandbox should be false")
	}
	if p.HITLAutoApprove {
		t.Error("standard: HITLAutoApprove should be false")
	}
	if !p.TaintEgressBlockEnabled {
		t.Error("standard: TaintEgressBlockEnabled should be true")
	}
	if !p.PIITokenizeEnabled {
		t.Error("standard: PIITokenizeEnabled should be true")
	}
	if !p.RunCmdWhitelistEnabled {
		t.Error("standard: RunCmdWhitelistEnabled should be true")
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
	if p.CodeActAllToolsAllowed {
		t.Error("standard: CodeActAllToolsAllowed should be false")
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
}

func TestProfilePolicy_Full(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileFull)
	if !p.SandboxDisabled {
		t.Error("full: SandboxDisabled should be true")
	}
	if p.ForceContainerSandbox {
		// SandboxDisabled 与 ForceContainerSandbox 互斥
		t.Error("full: ForceContainerSandbox must be false when SandboxDisabled=true")
	}
	if !p.AllowedPathsOpen {
		t.Error("full: AllowedPathsOpen should be true")
	}
	if !p.HITLAutoApprove {
		t.Error("full: HITLAutoApprove should be true")
	}
	if p.TaintEgressBlockEnabled {
		t.Error("full: TaintEgressBlockEnabled should be false (audit-only)")
	}
	if p.PIITokenizeEnabled {
		t.Error("full: PIITokenizeEnabled should be false")
	}
	if p.RunCmdWhitelistEnabled {
		t.Error("full: RunCmdWhitelistEnabled should be false")
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
	if !p.CodeActAllToolsAllowed {
		t.Error("full: CodeActAllToolsAllowed should be true")
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
}

func TestProfilePolicy_Hardened(t *testing.T) {
	p := policy.ProfilePolicy(policy.ProfileHardened)
	if p.SandboxDisabled {
		t.Error("hardened: SandboxDisabled should be false")
	}
	if !p.ForceContainerSandbox {
		t.Error("hardened: ForceContainerSandbox should be true")
	}
	if p.HITLAutoApprove {
		t.Error("hardened: HITLAutoApprove should be false")
	}
	if !p.TaintEgressBlockEnabled {
		t.Error("hardened: TaintEgressBlockEnabled should be true")
	}
	if !p.PIITokenizeEnabled {
		t.Error("hardened: PIITokenizeEnabled should be true")
	}
	if !p.RunCmdWhitelistEnabled {
		t.Error("hardened: RunCmdWhitelistEnabled should be true")
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
	if p.CodeActAllToolsAllowed {
		t.Error("hardened: CodeActAllToolsAllowed should be false")
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

// 确认 full 档的互斥约束：SandboxDisabled 和 ForceContainerSandbox 不同时 true。
func TestProfilePolicy_MutualExclusion(t *testing.T) {
	for _, prof := range []policy.TrustProfile{
		policy.ProfileStandard, policy.ProfileFull, policy.ProfileHardened,
	} {
		p := policy.ProfilePolicy(prof)
		if p.SandboxDisabled && p.ForceContainerSandbox {
			t.Errorf("profile %q: SandboxDisabled and ForceContainerSandbox cannot both be true", prof)
		}
	}
}
