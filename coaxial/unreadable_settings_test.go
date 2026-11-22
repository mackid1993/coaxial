package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A settings.json that could not be read was answered as a first run: the page offered the welcome setup, and
// saving it replaced the file without a word. The answer now carries the problem; with no file, it does not.
func TestUnreadableSettingsAreNotAFirstRun(t *testing.T) {
	dir := t.TempDir()
	l := &launcher{dir: dir, file: filepath.Join(dir, "settings.json"), quit: make(chan int, 4), prog: builtinProgram}
	ask := func() map[string]any {
		rec := httptest.NewRecorder()
		l.apiSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	l.problem = "There is no settings.json yet"
	if p, ok := ask()["problem"]; ok {
		t.Errorf("a first run is told %q", p)
	}
	os.WriteFile(l.file, []byte(`{"providers":[{"name":"A",}]}`), 0o600)
	l.problem = "settings.json could not be read: invalid character"
	if p, _ := ask()["problem"].(string); !strings.Contains(p, "could not be read") {
		t.Errorf("an unreadable settings.json is answered as a first run (problem %q)", p)
	}
}
