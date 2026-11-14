package pluginspec

import (
	"path/filepath"
	"testing"
)

func TestLoadExpertsValidatesExpertCatalog(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "agents", "lead.md"), "---\nname: lead\ndescription: lead\n---\nBody")
	writeFile(t, filepath.Join(tmp, "plugin.json"), `{"name":"sample","agents":["./agents/lead.md"],"extensions":{"ai.polarisagi":{"experts":[{"id":"research","type":"team","entryAgent":"lead","displayName":{"en":"Research"},"description":{"en":"Research team"},"quickPrompts":[{"en":"Find evidence"}],"members":["lead"]}]}}}`)
	p, err := Load(tmp, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Experts) != 1 || p.Experts[0].QuickPrompts[0]["en"] != "Find evidence" {
		t.Fatalf("unexpected experts: %#v", p.Experts)
	}
}
