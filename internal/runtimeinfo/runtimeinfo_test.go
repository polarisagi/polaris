package runtimeinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func newPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		PID:   filepath.Join(dir, "polaris.pid"),
		Port:  filepath.Join(dir, "polaris.port"),
		Token: filepath.Join(dir, "polaris.token"),
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	p := newPaths(t)
	token, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("令牌长度应为 %d，实际 %d", tokenBytes*2, len(token))
	}
	want := State{PID: 4242, Port: 28888, Token: token}
	if err = Write(p, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("往返不一致：want %+v got %+v", want, got)
	}
}

// 令牌文件必须落在 0600——它等价于完整 API 权限。
func TestWriteTokenPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不按 Unix 权限位判定")
	}
	p := newPaths(t)
	if err := Write(p, State{PID: 1, Port: 1234, Token: "abc"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	fi, err := os.Stat(p.Token)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != filePerm {
		t.Fatalf("令牌权限应为 %v，实际 %v", filePerm, fi.Mode().Perm())
	}
}

// 负向：权限被放宽的令牌文件必须拒绝读取，而不是宽容地读出来用。
func TestReadRejectsWidePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不按 Unix 权限位判定")
	}
	p := newPaths(t)
	if err := Write(p, State{PID: 1, Port: 1234, Token: "abc"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Chmod(p.Token, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, err := Read(p)
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("期望 CodeForbidden，实际 %v", err)
	}
}

// 负向：文件缺失必须是 CodeNotFound，调用方据此判定"守护进程未运行"。
func TestReadMissingIsNotFound(t *testing.T) {
	_, err := Read(newPaths(t))
	if !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("期望 CodeNotFound，实际 %v", err)
	}
}

// 负向：损坏/截断的端口文件必须是 CodeInvalidInput，不得被当成缺失。
func TestReadCorruptPort(t *testing.T) {
	p := newPaths(t)
	if err := os.WriteFile(p.Port, []byte("not-a-port"), filePerm); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Read(p)
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("期望 CodeInvalidInput，实际 %v", err)
	}
}

// 负向：端口越界在写入侧即拦截，避免把非法值扩散到客户端。
func TestWriteRejectsBadInput(t *testing.T) {
	p := newPaths(t)
	if err := Write(p, State{PID: 1, Port: 0, Token: "abc"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("端口 0 应被拒绝，实际 %v", err)
	}
	if err := Write(p, State{PID: 1, Port: 70000, Token: "abc"}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("端口 70000 应被拒绝，实际 %v", err)
	}
	if err := Write(p, State{PID: 1, Port: 1234, Token: ""}); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("空令牌应被拒绝，实际 %v", err)
	}
}

// PID 读不到不影响连接——它不参与控制流。
func TestReadWithoutPID(t *testing.T) {
	p := newPaths(t)
	if err := Write(p, State{PID: 99, Port: 28888, Token: "abc"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Remove(p.PID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Port != 28888 || got.Token != "abc" || got.PID != 0 {
		t.Fatalf("意外状态 %+v", got)
	}
}

// 原子写：反复重写不留临时文件残渣。
func TestWriteLeavesNoTemp(t *testing.T) {
	p := newPaths(t)
	for i := 0; i < 3; i++ {
		if err := Write(p, State{PID: i, Port: 20000 + i, Token: "tok"}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(p.Port))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("目录应只剩 3 个文件，实际 %v", names)
	}
}

func TestRemove(t *testing.T) {
	p := newPaths(t)
	if err := Write(p, State{PID: 1, Port: 1234, Token: "abc"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	Remove(p)
	for _, path := range []string{p.PID, p.Port} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s 应已删除", path)
		}
	}
	if _, err := os.Stat(p.Token); err != nil {
		t.Fatalf("令牌文件应在关停后保留以便重启复用: %v", err)
	}
	Remove(p) // 幂等
}

func TestLoadOrCreateToken(t *testing.T) {
	p := newPaths(t)

	first, reused, err := LoadOrCreateToken(p.Token)
	if err != nil || reused || !validToken(first) {
		t.Fatalf("首次应新建: tok=%q reused=%v err=%v", first, reused, err)
	}
	if err = Write(p, State{PID: 1, Port: 1234, Token: first}); err != nil {
		t.Fatal(err)
	}
	Remove(p)

	second, reused, err := LoadOrCreateToken(p.Token)
	if err != nil || !reused || second != first {
		t.Fatalf("重启应复用: tok=%q reused=%v err=%v", second, reused, err)
	}

	// 损坏内容 → 重新生成。
	if err = os.WriteFile(p.Token, []byte("not-a-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, reused, _ := LoadOrCreateToken(p.Token); reused || tok == "not-a-token" || !validToken(tok) {
		t.Fatalf("损坏令牌不应复用: tok=%q reused=%v", tok, reused)
	}

	// 权限放宽 → 重新生成（POSIX）。
	if runtime.GOOS != "windows" {
		if err = os.WriteFile(p.Token, []byte(first), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(p.Token, 0o644); err != nil {
			t.Fatal(err)
		}
		if tok, reused, _ := LoadOrCreateToken(p.Token); reused || tok == first {
			t.Fatalf("权限过宽的令牌不应复用: reused=%v", reused)
		}
	}
}

func TestRotateToken(t *testing.T) {
	p := newPaths(t)
	tok, _, _ := LoadOrCreateToken(p.Token)
	if err := Write(p, State{PID: 1, Port: 1234, Token: tok}); err != nil {
		t.Fatal(err)
	}
	if err := RotateToken(p.Token); err != nil {
		t.Fatal(err)
	}
	if err := RotateToken(p.Token); err != nil { // 幂等
		t.Fatal(err)
	}
	next, reused, _ := LoadOrCreateToken(p.Token)
	if reused || next == tok {
		t.Fatalf("轮换后应得到新令牌")
	}
}
