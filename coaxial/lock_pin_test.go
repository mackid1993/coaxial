package main

import (
	"testing"
	"time"
)

// With a provider locked to one address and no server chosen yet, the first choice is still made: until a
// server is named, gluetun takes one of the city's servers at random at every start of the VPN (container
// start, gluetun update, its own reconnect), so the address would change each time. Once one is named, it stays.
func TestLockedProviderGetsAServerNamed(t *testing.T) {
	old := switchWait
	switchWait = 2 * time.Second
	defer func() { switchWait = old }()
	fakePings(t, map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}, nil)
	playingSocket(t) // (the proxy part answers: nothing plays)
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	fakeServers(t, l)
	l.set.parse([]byte(`{"providers":[{"name":"One","advanced":{"locked_to_address":true}}],` +
		`"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	l.probeRound(true)
	l.switchServer(false)
	if _, tried := l.probeSkip["NY#2"]; !tried { // (the VPN cannot start here, so the move is tried and given up)
		t.Fatalf("no first choice was tried with a locked provider: %+v", l.probeSkip)
	}
	key := pickKey(l.set.vpnEnv())
	l.pick = picked{Key: key, Env: "SERVER_NAMES=NY#3", Name: "NY#3"}
	l.probeSkip = nil
	l.probeRound(true)
	l.better, l.betterWhy, l.betterRuns = &probeResult{Name: "NY#2", env: "SERVER_NAMES=NY#2", Median: 5}, "a test", 5
	l.switchServer(false)
	if l.pick.Env != "SERVER_NAMES=NY#3" || l.pickTry != "" || l.probeSkip != nil {
		t.Errorf("the server named was changed on Coaxial's own: %+v (skip %v)", l.pick, l.probeSkip)
	}
}
