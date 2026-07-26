package admin

import "testing"

func TestEmbeddedAdminTemplatesParse(t *testing.T) {
	tmpl, err := parseAdminTemplates()
	if err != nil {
		t.Fatalf("parse embedded templates: %v", err)
	}
	for _, name := range []string{
		"login.html",
		"dashboard.html",
		"users.html",
		"relay.html",
		"relay_edit.html",
		"relay_credentials.html",
		"relay_credentials_edit.html",
		"relay_models.html",
		"relay_models_edit.html",
		"relay_binding_edit.html",
		"relay_dashboard.html",
		"relay_logs.html",
	} {
		if tmpl.Lookup(name) == nil {
			t.Errorf("embedded template %q is missing", name)
		}
	}
}
