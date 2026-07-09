package templates

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed *.tmpl
var FS embed.FS

// Render 解析并执行指定的模板文件。
func Render(name string, data any) (string, error) {
	tmpl, err := template.ParseFS(FS, name)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
