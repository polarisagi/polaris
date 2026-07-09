package templates

import (
	"bytes"
	"embed"
	"text/template"

	"github.com/polarisagi/polaris/pkg/apperr"
)

//go:embed *.tmpl
var FS embed.FS

// Render 解析并执行指定的模板文件。
func Render(name string, data any) (string, error) {
	tmpl, err := template.ParseFS(FS, name)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to parse template", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to execute template", err)
	}
	return buf.String(), nil
}
