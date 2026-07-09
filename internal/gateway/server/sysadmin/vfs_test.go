package sysadmin

import (
	"testing"
)

func TestSanitizeUploadExt(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{".jpg", ".jpg"},
		{".PNG", ".png"},
		{".exe", ".blob"},
		{".sh", ".blob"},
		{"", ".blob"},
		{"pdf", ".blob"},
		{".tar.gz", ".blob"},
		{"..php", ".blob"},
		{".php.jpg", ".blob"}, // contains "." after first
	}

	for _, c := range cases {
		if out := sanitizeUploadExt(c.in); out != c.out {
			t.Errorf("sanitizeUploadExt(%q) = %q, expected %q", c.in, out, c.out)
		}
	}
}
