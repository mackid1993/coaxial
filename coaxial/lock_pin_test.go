package main

import (
	"net"
	"net/http"
	"sync/atomic"
	"testing"
)

// With a provider locked to one address and no server chosen yet, the picker still makes its first choice: until
// a server is named, gluetun takes one of the city's servers at random at every start of the VPN (container
// start, gluetun update, its own reconnect), so the address would change each time. Once one is named, it stays.
func TestLockedProviderGetsAServerNamed(t *testing.T) {
	ln, err := net.Listen("unix", insideSocket)
	if err != nil {
		t.Skip("the proxy part's socket is in use here")
	}
	var asked atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) >= 2 { // someone starts playing while the second server is about to be tried
			w.Write([]byte(`{"playing":[{"device":"tv"}]}`))
			return
		}
		w.Write([]byte(`{"playing":[]}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	measured := 0
	oldMeasure := measure
	defer func() { measure = oldMeasure }()
	measure = func(string, net.IP) (float64, float64, error) { measured++; return 100, 0, nil }

	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	l.set = &settings{}
	l.set.parse([]byte(`{"providers":[{"name":"One","advanced":{"locked_to_address":true}}],` +
		`"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	optionsMu.Lock()
	optionsKept, optionsKey = &vpnOptions{Updated: map[string]string{}}, "built in"
	optionsServers = map[string][]serverEntry{"fakevpn": {
		{VPN: "wireguard", City: "New York", ServerName: "NY#1"}, {VPN: "wireguard", City: "New York", ServerName: "NY#2"}}}
	optionsMu.Unlock()
	defer func() { optionsMu.Lock(); optionsKept, optionsKey, optionsServers = nil, "", nil; optionsMu.Unlock() }()

	l.pickServer("no server is chosen for these settings yet", false)
	if got := l.pickOverride(l.set); got == "" {
		t.Fatalf("no server is named to gluetun: it picks one at random at every start of the VPN (pick %+v)", l.pick)
	}
	was, at := l.pick, l.pickAt
	asked.Store(0)
	measured = 0
	l.pickServer("the last run was cut short", false) // (the next quiet hour)
	if measured != 0 || asked.Load() != 0 || l.pickTry != "" || l.pick != was || l.pickAt != at {
		t.Errorf("the server named was changed on Coaxial's own: %d measured, trying %q, now %+v", measured, l.pickTry, l.pick)
	}
}
