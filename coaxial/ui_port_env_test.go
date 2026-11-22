package main

import "testing"

// The web page's port, when settings.json names none (at the first start there is no file at all), is UI_PORT
// from .env, which the compose file passes on as COAXIAL_UI_PORT: Docker opens that port, so the page must be
// there. A port saved in the settings wins; a value that is no port, or one used inside the container, means 8195.
func TestUIPortFromEnvWhenUnset(t *testing.T) {
	for _, c := range []struct{ env, file, want string }{
		{"19010", `{}`, "19010"},
		{"19010", `{"ui":{"port":8300}}`, "8300"},
		{"", `{}`, "8195"},
		{"abc", `{}`, "8195"},
		{"8000", `{}`, "8195"},
	} {
		t.Setenv("COAXIAL_UI_PORT", c.env)
		s := &settings{}
		if err := s.parse([]byte(c.file)); err != nil || s.UI.Port.String() != c.want {
			t.Errorf("COAXIAL_UI_PORT=%q, %s: port %q (%v), want %s", c.env, c.file, s.UI.Port, err, c.want)
		}
	}
	t.Setenv("COAXIAL_UI_PORT", "19010")
	if got := emptySettings().masked()["ui"].(map[string]any)["port"]; got != port("19010") {
		t.Errorf("the page offers port %v at the first start, want 19010", got)
	}
}
