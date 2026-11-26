package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// With a city picked and no server named, Coaxial chooses the VPN server itself, and that choice is part of
// what the VPN was started with. The save from the page compared the new settings with it and left the choice
// out, so every save looked like a change of the VPN: the page said "The proxy is restarting" when nothing
// was started again, and a change of the Log detail was never passed to the proxy part, which went on writing
// at the old level until its next start.
func TestSaveWithAChosenServerSaysWhatRestarts(t *testing.T) {
	var mux sync.Mutex
	var asked []string
	ln, err := net.Listen("unix", insideSocket) // the proxy part, as the launcher asks it
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.Lock()
		asked = append(asked, r.URL.RequestURI())
		mux.Unlock()
		w.Write([]byte("ok\n"))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	old := pageSaved(goodSettings()) // (a city is picked, no server is named)
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, set: old, uiPort: 8195, pickLoaded: true}
	l.file = filepath.Join(l.dir, "settings.json")
	l.pick = picked{Key: pickKey(old.vpnEnv()), Env: "SERVER_NAMES=NY#1", Name: "NY#1"}
	if l.pick.Key == "" {
		t.Fatal("the settings of this test leave no server to choose")
	}
	_, l.vpnKey, _ = l.gluetunEnv(old)
	l.vpnKey += "picked " + l.pickOverride(old) + "\n" // as apply started the VPN
	l.proxyKey = string(old.forProxy())
	l.vpn = &child{done: make(chan struct{})} // (the VPN runs: it must not be stopped)
	keptLevel := currentLogLevel()
	defer setLogLevel(keptLevel)

	m := old.masked()
	m["ui"].(map[string]any)["log_level"] = "quiet" // the one thing changed on the page
	data, _ := json.Marshal(m)
	rec := httptest.NewRecorder()
	l.apiSettings(rec, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(data))))
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 {
		t.Fatalf("saved: %d %v", rec.Code, got)
	}
	if r, _ := got["restarting"].(map[string]any); r["vpn"] != false || r["proxy"] != false {
		t.Errorf("only the log detail changed and the page is told this restarts: %v", r)
	}
	time.Sleep(500 * time.Millisecond) // (what the save starts in the background)
	l.mu.Lock()
	stopped := l.vpn == nil || l.vpn.stopped
	l.mu.Unlock()
	if stopped {
		t.Errorf("the VPN was stopped by a change of the log detail")
	}
	mux.Lock()
	defer mux.Unlock()
	told := false
	for _, a := range asked {
		told = told || a == "/log-level?to=quiet"
	}
	if !told {
		t.Errorf("the proxy part keeps running and was not told the new log detail (it was asked %v): it writes at the old level until its next start", asked)
	}
}
