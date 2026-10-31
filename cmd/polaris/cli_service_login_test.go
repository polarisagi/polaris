package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type loginTools struct {
	task, autorun                               bool
	createFails, runFails, deleteFails          bool
	registryFails, launchFails, unregisterFails bool
	calls                                       []string
}

func (f *loginTools) run(name string, args ...string) error {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	failure := apperr.New(apperr.CodeInternal, "mock Windows permission error")
	switch name + " " + args[0] {
	case "schtasks /query":
		if !f.task {
			return failure
		}
	case "schtasks /create":
		if f.createFails {
			return failure
		}
		f.task = true
	case "schtasks /run":
		if f.runFails {
			return failure
		}
	case "schtasks /delete":
		if f.deleteFails {
			return failure
		}
		f.task = false
	case "reg.exe query":
		if !f.autorun {
			return failure
		}
	case "reg.exe add":
		if f.registryFails {
			return failure
		}
		f.autorun = true
	case "reg.exe delete":
		if f.unregisterFails {
			return failure
		}
		f.autorun = false
	case "powershell.exe -NoProfile":
		if f.launchFails {
			return failure
		}
	}
	return nil
}

func TestWindowsLoginInstall_Backends(t *testing.T) {
	for _, tc := range []struct {
		name              string
		tools             loginTools
		wantTask, wantRun bool
		wantError         bool
	}{
		{name: "scheduler", wantTask: true},
		{name: "standard user", tools: loginTools{createFails: true}, wantRun: true},
		{name: "task run failure", tools: loginTools{runFails: true}, wantRun: true},
		{name: "existing user entry", tools: loginTools{autorun: true}, wantRun: true},
		{name: "migrate duplicate", tools: loginTools{task: true, autorun: true}, wantRun: true},
		{name: "task cleanup denied", tools: loginTools{runFails: true, deleteFails: true}, wantTask: true, wantError: true},
		{name: "both backends denied", tools: loginTools{createFails: true, registryFails: true}, wantError: true},
		{name: "launch denied", tools: loginTools{createFails: true, launchFails: true}, wantRun: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.tools
			err := windowsLoginInstall(`C:\Users\张三 O'Brien\$work\polaris-service.ps1`, f.run)
			if (err != nil) != tc.wantError || f.task != tc.wantTask || f.autorun != tc.wantRun {
				t.Fatalf("err=%v, task=%v, autorun=%v; calls=%v", err, f.task, f.autorun, f.calls)
			}
			if tc.wantRun && !tc.wantError {
				registered, unit := windowsServiceRegistration(f.run)
				if !registered || !strings.HasPrefix(unit, windowsRunKey) {
					t.Fatalf("HKCU 自启动未被状态查询识别: %v %s", registered, unit)
				}
				f.calls = nil
				if err := windowsLoginInstall(`C:\Users\Owner\polaris-service.ps1`, f.run); err != nil {
					t.Fatal(err)
				}
				if slices.ContainsFunc(f.calls, func(call string) bool { return strings.HasPrefix(call, "schtasks /create") }) {
					t.Fatalf("重复安装创建了第二个后端: %v", f.calls)
				}
			}
		})
	}
}

func TestWindowsLoginUninstall_Backends(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tools     loginTools
		wantError bool
	}{
		{name: "missing"},
		{name: "scheduler", tools: loginTools{task: true}},
		{name: "standard user", tools: loginTools{autorun: true}},
		{name: "both", tools: loginTools{task: true, autorun: true}},
		{name: "delete denied", tools: loginTools{autorun: true, unregisterFails: true}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.tools
			err := windowsLoginUninstall(`C:\Users\Owner\polaris.exe`, f.run)
			if (err != nil) != tc.wantError || (!tc.wantError && (f.task || f.autorun)) {
				t.Fatalf("err=%v, task=%v, autorun=%v; calls=%v", err, f.task, f.autorun, f.calls)
			}
			if !tc.wantError {
				registered, _ := windowsServiceRegistration(f.run)
				if registered {
					t.Fatal("卸载后仍报告已注册")
				}
				stop := f.calls[len(f.calls)-3]
				if !strings.Contains(stop, "$_.Path -eq 'C:\\Users\\Owner\\polaris.exe'") || !strings.Contains(stop, "$_.Id -ne ") {
					t.Fatalf("停止进程未限制安装路径或排除本 CLI: %s", stop)
				}
			}
		})
	}
}

func TestWindowsLoginInstall_CommandEscapingAndLimit(t *testing.T) {
	f := loginTools{createFails: true}
	if err := windowsLoginInstall(`C:\Users\O'Brien\$work\polaris-service.ps1`, f.run); err != nil {
		t.Fatal(err)
	}
	launch := f.calls[len(f.calls)-1]
	if !strings.Contains(launch, `O''Brien\$work`) || !strings.Contains(launch, "-WindowStyle Hidden") || strings.Contains(launch, "RunAs") {
		t.Fatalf("隐藏启动转义或权限错误: %s", launch)
	}
	f = loginTools{createFails: true}
	if err := windowsLoginInstall(strings.Repeat("长", 260), f.run); err == nil || f.autorun {
		t.Fatalf("超长 Run 命令未拒绝: %v", err)
	}
}
