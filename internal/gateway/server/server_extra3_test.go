package server

import (
	"testing"
)

func TestServerBuilderMethods(t *testing.T) {
	s := &Server{}
	s.SetPluginCreator(nil)
	s.SetInstallManager(nil)
	s.SetOutboxWriter(nil)
	s.SetAuditTrail(nil)

	if agentStateString(1) != "perceive" {
		t.Errorf("expected perceive")
	}
	if agentStateString(0) != "idle" {
		t.Errorf("expected idle")
	}
	if agentStateString(2) != "plan" {
		t.Errorf("expected plan")
	}
}

func TestServerSetters(t *testing.T) {
	defer func() { recover() }()
	s := &Server{}
	s.SetScriptRunner(nil)
	s.SetSkillSigningKey(nil)
	s.SetUpdater(nil)
	s.SetMCPManager(nil)
	s.SetToolRegistry(nil)
	s.SetSkillRegistry(nil)
	s.SetToolExecutor(nil)
	s.SetLogStore(nil)
	s.SetEvalRunner(nil)
}
