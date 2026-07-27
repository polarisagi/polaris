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
		{"sub directory", "sub/file.md", true},
		{"backslash path", "sub\\file.md", true},
		{"just dots", "..", true},
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
