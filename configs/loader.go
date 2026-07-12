package configs

import (
	"bytes"
	"fmt"
	"path/filepath"
	"text/template"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// LoadPromptTemplate loads a template from the embedded FS and executes it with the given data.
func LoadPromptTemplate(name string, data any) (string, error) {
	// The path in embedded FS requires forward slashes
	fullPath := filepath.ToSlash(filepath.Join("prompts", name))

	content, err := FS.ReadFile(fullPath)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeNotFound, fmt.Sprintf("failed to read prompt template %q", fullPath), err)
	}

	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to parse prompt template %q", fullPath), err)
	}

	if data == nil {
		// If no data, return the raw parsed string (or rather just the original string
		// if we didn't want to parse, but parsing ensures it's a valid template).
		return string(content), nil
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to execute prompt template %q", fullPath), err)
	}

	return buf.String(), nil
}
