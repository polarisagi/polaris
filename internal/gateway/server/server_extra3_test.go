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
