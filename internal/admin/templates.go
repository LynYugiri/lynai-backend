package admin

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templateFS embed.FS

func adminTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"divPercent": func(value, total int64) float64 {
			if total == 0 {
				return 0
			}
			return float64(value) * 100 / float64(total)
		},
		"add": func(value, delta int) int { return value + delta },
		"sub": func(value, delta int) int { return value - delta },
	}
}

func parseAdminTemplates() (*template.Template, error) {
	return template.New("").Funcs(adminTemplateFuncs()).ParseFS(templateFS, "templates/*.html")
}
