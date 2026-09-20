package codeact

import "testing"

// GR-4.2-002：from-import 形式与调用先于导入的写法不得绕过 L0。
func TestCheckPython_FromImportBypass(t *testing.T) {
	d := &DefaultASTChecker{}
	blocked := []string{
		"from os import system\nsystem('ls')\n",
		"from subprocess import Popen as P\nP(['ls'])\n",
		"from os import *\n",
		"def f():\n    sp('x')\nfrom os import popen as sp\n",
		"import posix\nposix.system('ls')\n",
	}
	for _, code := range blocked {
		if err := d.CheckPython([]byte(code)); err == nil {
			t.Errorf("expected block:\n%s", code)
		}
	}
	allowed := []string{
		"from os import path\nprint(path.join('a','b'))\n",
		"import json\njson.dumps({})\n",
	}
	for _, code := range allowed {
		if err := d.CheckPython([]byte(code)); err != nil {
			t.Errorf("unexpected block for %q: %v", code, err)
		}
	}
}

// GR-4.2-008：只解析选项位——文件名含 -f/-r 放行，-R/--recursive/--force 拦截。
func TestCheckBash_RMFlags(t *testing.T) {
	d := &DefaultASTChecker{}
	for _, code := range []string{"rm my-file.txt", "rm auto-fix.log", "rm -- -rf-named-file", "rm -i a"} {
		if err := d.CheckBash([]byte(code)); err != nil {
			t.Errorf("%q should pass: %v", code, err)
		}
	}
	for _, code := range []string{"rm -rf /tmp/x", "rm -R dir", "rm --recursive dir", "rm --force a", "rm -f a"} {
		if err := d.CheckBash([]byte(code)); err == nil {
			t.Errorf("%q should be blocked", code)
		}
	}
}
