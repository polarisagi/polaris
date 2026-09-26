package pluginspec

import (
	"reflect"
	"testing"
)

func TestParseSourceSpec(t *testing.T) {
	cases := map[string]PluginSource{
		"org/fmt":               {Type: SourceGitHub, Repo: "org/fmt"},
		"org/fmt@v2":            {Type: SourceGitHub, Repo: "org/fmt", Ref: "v2"},
		"org/mono/tools/x#main": {Type: SourceGitSubdir, URL: "https://github.com/org/mono.git", Path: "tools/x", Ref: "main"},
		"https://github.com/openai/skills/tree/main/skills/.curated/pdf": {Type: SourceGitSubdir,
			URL: "https://github.com/openai/skills.git", Ref: "main", Path: "skills/.curated/pdf"},
		"https://gitlab.example/g/r.git#v1": {Type: SourceURL, URL: "https://gitlab.example/g/r.git", Ref: "v1"},
		"https://a.example/p.zip":           {Type: SourceArchive, URL: "https://a.example/p.zip"},
		"npm:@org/fmt@^2.0.0":               {Type: SourceNPM, Package: "@org/fmt", Version: "^2.0.0"},
		"npm:fmt":                           {Type: SourceNPM, Package: "fmt"},
		"/srv/plugins/fmt":                  {Type: SourceRelative, Path: "/srv/plugins/fmt"},
	}
	for in, want := range cases {
		got, err := ParseSourceSpec(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "git@github.com:o/r.git", "file:///etc", "http://x/y", "justname", "org/r/../x", "/a/../b"} {
		if _, err := ParseSourceSpec(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
