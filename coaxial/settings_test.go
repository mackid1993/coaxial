package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// put: the page saving these settings (as it sends them: what it was given, passwords left blank) to a
// launcher that runs old.
func put(t *testing.T, old *settings, change func(m map[string]any)) (*launcher, int, map[string]any) {
	t.Helper()
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, set: old, uiPort: 8195}
	l.file = filepath.Join(l.dir, "settings.json")
	_, l.vpnKey, _ = l.gluetunEnv(old)
	l.proxyKey = string(old.forProxy())
	l.vpn = &child{done: make(chan struct{})} // (the VPN runs: it must not be stopped)
	m := old.masked()
	if change != nil {
		change(m)
	}
	data, _ := json.Marshal(m)
	rec := httptest.NewRecorder()
	l.apiSettings(rec, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(data))))
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	return l, rec.Code, got
}

// pageSaved: settings as a save from the page wrote them (every option written out).
func pageSaved(s *settings) *settings {
	data, _ := json.Marshal(s.masked())
	out := &settings{}
	out.parse(data)
	out.keepSecrets(s)
	return out
}

// Pressing Save on the page without changing anything starts nothing again: no stream or VPN connection is
// dropped for it.
func TestSaveUnchangedRestartsNothing(t *testing.T) {
	l, code, got := put(t, pageSaved(goodSettings()), nil)
	if code != 200 {
		t.Fatalf("saved unchanged: %d %v", code, got)
	}
	if r, _ := got["restarting"].(map[string]any); r["vpn"] != false || r["proxy"] != false {
		t.Errorf("an unchanged save restarts %v", r)
	}
	// What the save starts in the background (apply) is waited for until it is done, where it was given a
	// flat 200 ms: on a slow test machine the look below came before apply had decided anything, and a stop
	// made after it went unseen. apply notes what the proxy part is started with as soon as it has compared
	// the settings, and holds l.work until it has stopped and started what it meant to.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		l.mu.Lock()
		begun := l.proxyIn != ""
		l.mu.Unlock()
		if begun {
			break
		}
	}
	l.work.Lock()
	l.work.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.proxyIn == "" {
		t.Errorf("5 s after the save, what it starts in the background has not run")
	}
	if l.vpn == nil || l.vpn.stopped {
		t.Errorf("the VPN was stopped by an unchanged save")
	}
}

// A provider moved to a new address with the password left as "Saved" on the page: the saved password is not
// sent to the new address, and the page says so next to the password, instead of only asking for one.
func TestSavedPasswordAfterTheAddressChanges(t *testing.T) {
	setURL := func(u string) func(m map[string]any) {
		return func(m map[string]any) { m["providers"].([]any)[0].(map[string]any)["url"] = u }
	}
	for _, u := range []string{"https://PROVIDER.example/", "http://provider.example"} { // (the same server)
		if _, code, got := put(t, goodSettings(), setURL(u)); code != 200 {
			t.Errorf("%s: the same server, saved password: %d %v", u, code, got)
		}
	}
	for _, u := range []string{"http://new-domain.example", "https://provider.example:8443"} {
		_, code, got := put(t, goodSettings(), setURL(u))
		errs, _ := got["errors"].([]any)
		if code != 400 || len(errs) != 1 {
			t.Fatalf("%s: %d %v", u, code, got)
		}
		e := errs[0].(map[string]any)
		if e["field"] != "providers[0].password" || !strings.Contains(e["message"].(string), "saved") {
			t.Errorf("%s: the page is not told why the saved password is not used: %v", u, e)
		}
	}
}
