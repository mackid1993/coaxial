package main

import (
	"net"
	"net/http"
	"testing"
)

// After a run cut short by someone playing, the server it stopped on is kept for those settings only: once
// the settings name another city, it is not forced on gluetun (which then finds no server and stays down).
func TestPickStoppedNotForcedOnNewSettings(t *testing.T) {
	ln, err := net.Listen("unix", insideSocket)
	if err != nil {
		t.Skip("the proxy part's socket is in use here")
	}
	asked := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if asked >= 2 { // someone starts playing while the second server is about to be tried
			w.Write([]byte(`{"playing":[{"device":"tv"}]}`))
			return
		}
		w.Write([]byte(`{"playing":[]}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	oldMeasure := measure
	defer func() { measure = oldMeasure }()
	measure = func(string, net.IP) (float64, float64, error) { return 100, 0, nil }

	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	optionsMu.Lock()
	optionsKept, optionsKey = &vpnOptions{Updated: map[string]string{}}, "built in"
	optionsServers = map[string][]serverEntry{"fakevpn": {
		{VPN: "wireguard", City: "New York", ServerName: "NY#1"}, {VPN: "wireguard", City: "New York", ServerName: "NY#2"}}}
	optionsMu.Unlock()
	defer func() { optionsMu.Lock(); optionsKept, optionsKey, optionsServers = nil, "", nil; optionsMu.Unlock() }()

	l.pickServer("a test", false)
	if l.pick.Env == "" || !l.pick.Unfinished {
		t.Fatalf("the run was not cut short as planned: %+v", l.pick)
	}
	same := &settings{}
	same.parse([]byte(`{"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	if got := l.pickOverride(same); got != l.pick.Env {
		t.Errorf("the same settings: %q, want the server it stopped on, %q", got, l.pick.Env)
	}
	other := &settings{}
	other.parse([]byte(`{"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"Chicago","gluetun_version":"builtin"}}`))
	if got := l.pickOverride(other); got != "" {
		t.Errorf("another city: %q is still forced on gluetun", got)
	}
}
