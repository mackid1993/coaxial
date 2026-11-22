package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// "Find the fastest server" with nothing to choose between (a city with one server, or none the list knows)
// must not leave the launcher thinking servers are being tried: that stops update checks for good.
func TestPickNothingToChooseFrom(t *testing.T) {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4)}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"nosuchservice","type":"wireguard","server_cities":"Nowhere"}}`))
	w := httptest.NewRecorder()
	l.apiPick(w, httptest.NewRequest("POST", "/api/vpn/server/pick", nil))
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("the button was refused: %s", w.Body)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		l.mu.Lock()
		busy := l.pickBusy
		l.mu.Unlock()
		if !busy {
			return
		}
	}
	t.Errorf("still trying servers after the run ended")
}
