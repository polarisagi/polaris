package skill_test

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/cognition/skill"
)

func TestValidateJS(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		wantErr string
	}{
		{
			name: "safe pure function",
			code: `
				function add(a, b) {
					return a + b;
				}
				add(1, 2);
			`,
			wantErr: "",
		},
		{
			name: "forbid eval",
			code: `
				const s = "alert(1)";
				eval(s);
			`,
			wantErr: "dynamic execution is forbidden",
		},
		{
			name: "forbid new Function",
			code: `
				const f = new Function("a", "return a");
			`,
			wantErr: "dynamic execution is forbidden",
		},
		{
			name: "forbid require",
			code: `
				const fs = require("fs");
			`,
			wantErr: "nodejs built-ins are forbidden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := skill.ValidateJS(tt.code)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}
