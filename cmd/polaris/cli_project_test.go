package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchProjectByDir_LongestRootWinsAndSkipsArchived(t *testing.T) {
	sep := string(os.PathSeparator)
	outer := sep + filepath.Join("work", "repo")
	inner := filepath.Join(outer, "svc")
	projects := []cliProject{
		{ID: "default"},
		{ID: "outer", RootPath: outer},
		{ID: "inner", RootPath: inner},
		{ID: "gone", RootPath: filepath.Join(inner, "pkg"), Archived: true},
		{ID: "sibling", RootPath: outer + "-other"},
	}
	cases := map[string]string{
		outer:                               "outer",
		filepath.Join(outer, "docs"):        "outer",
		inner:                               "inner",
		filepath.Join(inner, "pkg", "deep"): "inner", // 归档项目不参与匹配
		outer + "-other":                    "sibling",
		outer + "x":                         "", // 前缀相同但不是子目录
		sep + "elsewhere":                   "",
	}
	for dir, want := range cases {
		got := matchProjectByDir(projects, dir)
		gotID := ""
		if got != nil {
			gotID = got.ID
		}
		if gotID != want {
			t.Errorf("matchProjectByDir(%q) = %q, want %q", dir, gotID, want)
		}
	}
}

func TestResolveProjectRef(t *testing.T) {
	projects := []cliProject{
		{ID: "prj_1", Name: "polaris"},
		{ID: "prj_2", Name: "dup"},
		{ID: "prj_3", Name: "dup"},
	}
	if p, err := resolveProjectRef(projects, "prj_1"); err != nil || p.ID != "prj_1" {
		t.Fatalf("按 ID: %v %v", p, err)
	}
	if p, err := resolveProjectRef(projects, "polaris"); err != nil || p.ID != "prj_1" {
		t.Fatalf("按名称: %v %v", p, err)
	}
	if _, err := resolveProjectRef(projects, "dup"); err == nil {
		t.Fatal("重名应报错要求改用 ID")
	}
	if _, err := resolveProjectRef(projects, "none"); err == nil {
		t.Fatal("不存在应报错")
	}
}

func TestParseChatArgs(t *testing.T) {
	o, err := parseChatArgs([]string{"-p", "polaris", "--session=s1", "hello", "world"})
	if err != nil {
		t.Fatal(err)
	}
	if o.projectRef != "polaris" || o.sessionID != "s1" || o.message != "hello world" {
		t.Fatalf("got %+v", o)
	}
	if o, _ := parseChatArgs(nil); o.message != "" {
		t.Fatalf("无参数应进入 REPL，got %+v", o)
	}
	if _, err := parseChatArgs([]string{"--bogus"}); err == nil {
		t.Fatal("未知参数应报错")
	}
	// 消息正文里出现的 "-x" 不应被当成参数
	if o, err := parseChatArgs([]string{"what", "is", "-x"}); err != nil || o.message != "what is -x" {
		t.Fatalf("got %+v %v", o, err)
	}
}

func TestParseProjectNewArgs(t *testing.T) {
	o, err := parseProjectNewArgs([]string{"demo", "--here", "--trust"})
	if err != nil {
		t.Fatal(err)
	}
	if o.name != "demo" || !filepath.IsAbs(o.root) || !o.trust {
		t.Fatalf("--here 应解析为 cwd 绝对路径，got %+v", o)
	}
	for _, bad := range [][]string{
		{},                                // 缺名称
		{"x", "--trust"},                  // 信任却无目录
		{"x", "--here", "--root", "/tmp"}, // 二选一
		{"x", "y"},                        // 多余参数
		{"x", "--bogus"},                  // 未知参数
	} {
		if _, err := parseProjectNewArgs(bad); err == nil {
			t.Errorf("%v 应报错", bad)
		}
	}
}
