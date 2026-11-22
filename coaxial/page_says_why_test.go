package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// VPN settings that apply() does not start the VPN with (a settings.json edited by hand): the status page said
// only "Waiting for the VPN to reconnect", for good. The answer now says why.
func TestStatusSaysWhyTheVPNIsNotStarted(t *testing.T) {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"player":{"user":"tv","password":"pw"},"vpn":{"service_provider":"custom","extra":{"VPN_UP_COMMAND":"id"}}}`))
	rec := httptest.NewRecorder()
	l.apiStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
	var got struct{ VPN struct{ Note string } }
	json.Unmarshal(rec.Body.Bytes(), &got)
	if !strings.Contains(got.VPN.Note, "vpn.extra.VPN_UP_COMMAND") {
		t.Errorf("the status page is not told why the VPN is not started: note %q", got.VPN.Note)
	}
}
