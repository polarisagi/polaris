package prompt

import (
	"testing"
)

func TestSafePromptName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid name", "identity.md", false},
		{"empty string", "", true},
		{"contains directory traversal", "../../../etc/passwd", true},
		{"absolute path", "/etc/passwd", true},
		{"sub directory", "sub/file.md", false},
		{"backslash path", "sub\\file.md", false},
		{"just dots", "..", true},
		{"inner traversal escaping root", "a/../../b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := safePromptName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("safePromptName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// GR-6.1-008：子目录提示词必须能写入（原实现只 MkdirAll prompts/ 根目录）。
func TestWriteUserPrompt_Subdirectory(t *testing.T) {
	pm := NewManager(t.TempDir(), nil)
	if err := pm.WriteUserPrompt("agents/deep/coder.md", "hello"); err != nil {
		t.Fatalf("WriteUserPrompt subdir: %v", err)
	}
}
