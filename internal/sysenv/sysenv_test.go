package sysenv

import (
	"strings"
	"testing"
)

func TestGetSystemInfo(t *testing.T) {
	info := GetSystemInfo()

	if info == nil {
		t.Fatal("GetSystemInfo() returned nil")
	}

	if info.OSName == "" {
		t.Error("Expected OSName to be populated")
	}

	if info.Architecture == "" {
		t.Error("Expected Architecture to be populated")
	}

	if info.CPUCores <= 0 {
		t.Error("Expected CPUCores to be > 0")
	}

	md := info.FormatMarkdown()
	if !strings.Contains(md, "System Environment") {
		t.Error("Markdown should contain header")
	}
	if !strings.Contains(md, info.OSName) {
		t.Error("Markdown should contain OSName")
	}
}
