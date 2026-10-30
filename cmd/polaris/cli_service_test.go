package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServicePATH_ExactEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(home, ".polaris", "bin")
	t.Setenv("PATH", strings.Join([]string{installDir + "-old", localBin + "-old"}, string(os.PathListSeparator)))
	parts := filepath.SplitList(servicePATH(installDir))
	if len(parts) != 4 || parts[0] != installDir || parts[1] != localBin {
		t.Fatalf("服务 PATH 未准确前置、去重: %q", parts)
	}
	preferred := filepath.Join(home, "preferred-python")
	t.Setenv("PATH", strings.Join([]string{preferred, localBin, localBin}, string(os.PathListSeparator)))
	parts = filepath.SplitList(servicePATH(installDir))
	if len(parts) != 3 || parts[1] != preferred || parts[2] != localBin {
		t.Fatalf("服务 PATH 改变已验证的解释器优先级: %q", parts)
	}
}

func TestWindowsServiceScript_LiteralPaths(t *testing.T) {
	exe := `C:\Users\张三 O'Brien\$work\polaris.exe`
	path := `C:\Users\张三 O'Brien\bin;C:\Windows\System32`
	got := windowsServiceScript(exe, path)
	want := "\ufeff$env:Path = 'C:\\Users\\张三 O''Brien\\bin;C:\\Windows\\System32'\r\n" +
		"& 'C:\\Users\\张三 O''Brien\\$work\\polaris.exe' serve\r\nexit $LASTEXITCODE\r\n"
	if got != want {
		t.Fatalf("Windows 服务脚本转义错误: %q", got)
	}
}

func TestSystemdQuote_PathCharacters(t *testing.T) {
	if got, want := systemdQuote("PATH=/home/a b/100%/bin"), `"PATH=/home/a b/100%%/bin"`; got != want {
		t.Fatalf("systemd 路径转义错误: %q, want %q", got, want)
	}
}
