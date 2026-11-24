package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakePings answers pings by address: each address has its answer time and how many of every 10 it loses.
func fakePings(t *testing.T, ms map[string]float64, lose map[string]int) {
	oldPing, oldAllow := pingIP, allowPings
	t.Cleanup(func() { pingIP, allowPings = oldPing, oldAllow })
	allowPings = func() error { return nil }
	pingIP = func(_ context.Context, ip string, count int) ([]time.Duration, int, error) {
		var rtts []time.Duration
		for i := 0; i < count-lose[ip]*count/10; i++ {
			rtts = append(rtts, time.Duration((ms[ip]+float64(i%3)*0.1)*float64(time.Millisecond)))
		}
		return rtts, count, nil
	}
}

func fakeServers(t *testing.T, l *launcher) {
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	optionsMu.Lock()
	optionsKept, optionsKey = &vpnOptions{Updated: map[string]string{}}, "built in"
	optionsServers = map[string][]serverEntry{"fakevpn": {
		{VPN: "wireguard", City: "New York", ServerName: "NY#1", IPs: []string{"192.0.2.1"}},
		{VPN: "wireguard", City: "New York", ServerName: "NY#2", IPs: []string{"192.0.2.2"}},
		{VPN: "wireguard", City: "New York", ServerName: "NY#3", IPs: []string{"192.0.2.3"}},
		{VPN: "wireguard", City: "Chicago", ServerName: "CHI#1", IPs: []string{"192.0.2.9"}}}}
	optionsMu.Unlock()
	t.Cleanup(func() { optionsMu.Lock(); optionsKept, optionsKey, optionsServers = nil, "", nil; optionsMu.Unlock() })
}

// Servers losing pings come last, then the rest by answer time; one round of loss is not yet "bad".
func TestProbeRanking(t *testing.T) {
	fakePings(t, map[string]float64{"192.0.2.1": 20, "192.0.2.2": 5, "192.0.2.3": 3}, map[string]int{"192.0.2.3": 5})
	l := &launcher{dir: t.TempDir()}
	cands := []candidate{{name: "A", env: "SERVER_NAMES=A", ips: []string{"192.0.2.1"}},
		{name: "B", env: "SERVER_NAMES=B", ips: []string{"192.0.2.2"}}, {name: "C", env: "SERVER_NAMES=C", ips: []string{"192.0.2.3"}}}
	got := l.probeServers(cands, 10)
	if len(got) != 3 || got[0].Name != "B" || got[1].Name != "A" || got[2].Name != "C" {
		t.Fatalf("order: %+v", got)
	}
	if got[2].Loss != 50 || got[2].Bad {
		t.Errorf("C after one lossy round: loss %.0f, bad %v (want 50, not yet bad)", got[2].Loss, got[2].Bad)
	}
	if got = l.probeServers(cands, 10); !got[2].Bad {
		t.Errorf("C after two lossy rounds is not bad: %+v", got[2])
	}
}

// A server only somewhat faster than the one in use is moved to after two full rounds, not one.
func TestBetterAfterTwoRounds(t *testing.T) {
	fakePings(t, map[string]float64{"192.0.2.1": 4, "192.0.2.2": 30, "192.0.2.3": 25}, nil)
	l := &launcher{dir: t.TempDir(), pickLoaded: true}
	fakeServers(t, l)
	l.pick = picked{Key: pickKey(l.set.vpnEnv()), Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
	l.probeRound(true)
	if l.better == nil || l.better.Name != "NY#1" || !strings.Contains(l.betterWhy, "answers in") {
		t.Fatalf("better: %+v (%q)", l.better, l.betterWhy)
	}
	if l.betterReady() {
		t.Errorf("ready to move after one round")
	}
	l.probeRound(true)
	if !l.betterReady() {
		t.Errorf("not ready to move after two rounds")
	}
	if len(l.probeList) != 3 {
		t.Errorf("%d servers pinged, want the 3 in New York", len(l.probeList))
	}
	if st := l.pickStatus(); st["checked_at"] == nil || st["better"] == nil || st["now"] == nil {
		t.Errorf("status: %v", st)
	}
}

// A server in use that keeps losing pings is replaced as soon as nothing plays; a close race is left alone.
func TestLossyServerInUse(t *testing.T) {
	fakePings(t, map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}, map[string]int{"192.0.2.2": 3})
	l := &launcher{dir: t.TempDir(), pickLoaded: true}
	fakeServers(t, l)
	l.pick = picked{Key: pickKey(l.set.vpnEnv()), Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
	l.probeRound(true)
	if l.betterReady() {
		t.Errorf("ready after one lossy round")
	}
	l.probeRound(false) // (the minute check of the one in use)
	if !l.betterReady() || l.better == nil || !strings.Contains(l.betterWhy, "losing pings") {
		t.Errorf("not ready after two lossy rounds: %+v %q", l.better, l.betterWhy)
	}
	// NY#1 is in use and NY#3 only 1 ms behind: nothing to do.
	fakePings(t, map[string]float64{"192.0.2.1": 6, "192.0.2.2": 50, "192.0.2.3": 5}, nil)
	l2 := &launcher{dir: t.TempDir(), pickLoaded: true}
	fakeServers(t, l2)
	l2.pick = picked{Key: pickKey(l2.set.vpnEnv()), Env: "SERVER_NAMES=NY#1", Name: "NY#1"}
	l2.probeRound(true)
	l2.probeRound(true)
	if l2.better != nil {
		t.Errorf("a 1 ms difference called for a move: %+v", l2.better)
	}
}

// With no server chosen yet for the settings, the best one is ready at once.
func TestFirstChoiceAtOnce(t *testing.T) {
	fakePings(t, map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}, nil)
	l := &launcher{dir: t.TempDir(), pickLoaded: true}
	fakeServers(t, l)
	l.probeRound(true)
	if !l.betterReady() || l.better == nil || l.better.Name != "NY#2" {
		t.Errorf("no first choice: %+v", l.better)
	}
}

// A server that does not connect is given up on: the VPN goes back and that server is left out for a while.
func TestSwitchThatFailsGoesBack(t *testing.T) {
	old := switchWait
	switchWait = 2 * time.Second
	defer func() { switchWait = old }()
	fakePings(t, map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}, nil)
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	fakeServers(t, l)
	l.probeRound(true)
	l.switchServer(true)
	if _, left := l.probeSkip["NY#2"]; !left || l.pickTry != "" || l.pick.Env != "" || l.better != nil {
		t.Errorf("after a server that did not connect: skip %v, trying %q, chosen %q, better %+v", l.probeSkip, l.pickTry, l.pick.Env, l.better)
	}
	l.probeRound(true)
	if l.better == nil || l.better.Name == "NY#2" {
		t.Errorf("the server that did not connect is offered again: %+v", l.better)
	}
}
