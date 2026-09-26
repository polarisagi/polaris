package pluginspec

import "testing"

func TestFindAndReplaceInjections(t *testing.T) {
	body := "## Context\n- Diff: !`git diff --stat`\n- Not run: KEY=!`whoami`\n\n```!\nnode --version\ngit status --short\n```\nEnd !`echo done`"
	inj := FindInjections(body)
	if len(inj) != 3 {
		t.Fatalf("want 3 injections, got %+v", inj)
	}
	if inj[0].Command != "git diff --stat" || inj[1].Command != "node --version\ngit status --short" || inj[2].Command != "echo done" {
		t.Fatalf("commands: %q %q %q", inj[0].Command, inj[1].Command, inj[2].Command)
	}
	got := ReplaceInjections(body, inj, []string{"A", "B", "C"})
	want := "## Context\n- Diff: A\n- Not run: KEY=!`whoami`\n\nB\nEnd C"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if InjectionDigest(inj) == InjectionDigest(inj[:2]) {
		t.Fatal("digest must change with command set")
	}
}
