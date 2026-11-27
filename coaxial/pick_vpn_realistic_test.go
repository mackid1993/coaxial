package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// vrSocket serves the proxy part's inside socket (what the launcher asks "is anything playing?") with nothing
// playing. play puts one recorder on a channel; crash closes the socket, as when the proxy part has stopped and
// is being started again.
func vrSocket(t *testing.T) (play, crash func()) {
	ln, err := net.Listen("unix", insideSocket)
	if err != nil {
		t.Skip("the proxy part's socket is in use here")
	}
	srv := &http.Server{Handler: http.HandlerFunc(insideStatus)}
	go srv.Serve(ln)
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films
	channels, hlsRows, films = map[string]*channel{}, map[string]*hlsRow{}, map[string]*film{}
	mu.Unlock()
	var once sync.Once
	crash = func() { once.Do(func() { srv.Close(); ln.Close() }) }
	t.Cleanup(func() {
		crash()
		mu.Lock()
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		mu.Unlock()
	})
	return func() {
		ch := &channel{key: "/ts vrtest 2", readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.8"}: true}}
		mu.Lock()
		channels[ch.key] = ch
		mu.Unlock()
	}, crash
}

// vrPings answers pings by address with what answer(ip, round, ping) says (round: how many rounds that address
// was in before). lost: no answer to that ping.
func vrPings(t *testing.T, answer func(ip string, round, ping int) (ms float64, lost bool)) {
	oldPing, oldAllow := pingIP, allowPings
	t.Cleanup(func() { pingIP, allowPings = oldPing, oldAllow })
	allowPings = func() error { return nil }
	var mu sync.Mutex
	rounds := map[string]int{}
	pingIP = func(_ context.Context, ip string, count int) ([]time.Duration, int, error) {
		mu.Lock()
		r := rounds[ip]
		rounds[ip]++
		mu.Unlock()
		var rtts []time.Duration
		for i := 0; i < count; i++ {
			if ms, lost := answer(ip, r, i); !lost {
				rtts = append(rtts, time.Duration(ms*float64(time.Millisecond)))
			}
		}
		return rtts, count, nil
	}
}

// vrPicker: a launcher with the three New York servers of fakeServers on offer, NY#2 in use when inUse.
func vrPicker(t *testing.T, inUse bool) *launcher {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	fakeServers(t, l)
	if inUse {
		l.pick = picked{Key: pickKey(l.set.vpnEnv()), Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
	}
	return l
}

// Busy servers: the one that answers first changes from round to round (NY#1 one hour, NY#3 the next), each
// about 20 ms ahead of the one in use. Neither has been ahead two full rounds running, so the VPN must stay
// where it is: moving on one good round each time would start the VPN again every hour or two, all night.
func TestVRPickTakesTurnsBetweenBusyServersAndStays(t *testing.T) {
	vrPings(t, func(ip string, round, ping int) (float64, bool) {
		near, far := 10.0+float64(ping%3), 27.0+float64(ping%4)
		switch ip {
		case "192.0.2.1":
			if round%2 == 0 {
				return near, false
			}
			return far, false
		case "192.0.2.3":
			if round%2 == 1 {
				return near, false
			}
			return far, false
		}
		return 30 + float64(ping%3), false
	})
	l := vrPicker(t, true)
	for round := 1; round <= 4; round++ {
		l.probeRound(true)
		l.mu.Lock()
		ready, best := l.betterReady(), ""
		if l.better != nil {
			best = l.better.Name
		}
		l.mu.Unlock()
		if best == "" {
			t.Fatalf("round %d: no server was found better although one answers 20 ms sooner", round)
		}
		if ready {
			t.Errorf("round %d: ready to move to %s, which was ahead for one round only (the one ahead takes turns)", round, best)
		}
	}
}

// Real answer times spread: a busy server in use answers in 20 to 60 ms (median 23), a quieter one in 9 to 30 ms
// (median 10). 13 ms is inside the margin (probeCloser), so nothing is better, round after round. A server that
// really is closer (median 45 against 12, with the same spread) is ready after two full rounds and not after one.
func TestVRPickSpreadInsideTheMarginStays(t *testing.T) {
	cur := []float64{20, 22, 35, 21, 24, 60, 23, 22, 21, 25}
	quiet := []float64{9, 10, 11, 9, 30, 10, 12, 10, 9, 11}
	far := 0.0
	vrPings(t, func(ip string, round, ping int) (float64, bool) {
		switch ip {
		case "192.0.2.1":
			return quiet[ping%10] + 2, false
		case "192.0.2.2":
			return cur[ping%10] + far, false
		}
		return 40 + float64(ping%5), false
	})
	l := vrPicker(t, true)
	for round := 1; round <= 3; round++ {
		l.probeRound(true)
		if l.better != nil {
			t.Fatalf("round %d: %s called better (%s); medians 12 and 23 ms are inside the %.0f ms margin", round, l.better.Name, l.betterWhy, probeCloser)
		}
	}
	far = 22 // the server in use now answers in about 45 ms
	l = vrPicker(t, true)
	l.probeRound(true)
	if l.better == nil || l.better.Name != "NY#1" {
		t.Fatalf("a server 33 ms closer was not found better: %+v", l.better)
	}
	if l.betterReady() {
		t.Errorf("ready to move after one round")
	}
	l.probeRound(true)
	if !l.betterReady() {
		t.Errorf("a server 33 ms closer two full rounds running is not ready")
	}
}

// A server that cannot be reached at all: the pinger for it hangs until the round's own time limit. The round
// must end on that limit (probeWait, the pings, and 5 s more), count that server as losing every ping and rank
// it last, and still rank the others: the page's table and the picker are not held up by one dead address.
// (About 11 s.)
func TestVRPickRoundEndsWithAnUnreachableServer(t *testing.T) {
	if testing.Short() {
		t.Skip("about 11 s")
	}
	oldPing, oldAllow := pingIP, allowPings
	defer func() { pingIP, allowPings = oldPing, oldAllow }()
	allowPings = func() error { return nil }
	pingIP = func(ctx context.Context, ip string, count int) ([]time.Duration, int, error) {
		if ip == "192.0.2.3" {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(40 * time.Second):
				return nil, 0, context.DeadlineExceeded
			}
		}
		ms := map[string]float64{"192.0.2.1": 8, "192.0.2.2": 30}[ip]
		var rtts []time.Duration
		for i := 0; i < count; i++ {
			rtts = append(rtts, time.Duration(ms*float64(time.Millisecond)))
		}
		return rtts, count, nil
	}
	l := vrPicker(t, true)
	began := time.Now()
	l.probeRound(true)
	took := time.Since(began)
	if limit := probeWait + probeCount*probeGap + 5*time.Second + 2*time.Second; took > limit {
		t.Errorf("a ping round with one unreachable server took %v; its limit is %v", took.Round(100*time.Millisecond), limit-2*time.Second)
	}
	if len(l.probeList) != 3 || l.probeList[2].Name != "NY#3" || l.probeList[2].Loss != 100 {
		t.Fatalf("the unreachable server is not last with every ping lost: %+v", l.probeList)
	}
	if l.better == nil || l.better.Name != "NY#1" {
		t.Errorf("the others were not ranked: better %+v", l.better)
	}
}

// Pings filtered on the way (some networks drop every ping): every server loses every ping, round after round.
// No server is better than another then, whether one is in use or none is chosen yet: a move on that would be a
// VPN restart chosen by chance.
func TestVRPickAllPingsLostMovesNowhere(t *testing.T) {
	vrPings(t, func(string, int, int) (float64, bool) { return 0, true })
	for _, inUse := range []bool{false, true} {
		l := vrPicker(t, inUse)
		for round := 1; round <= 3; round++ {
			l.probeRound(true)
			if l.better != nil || l.betterReady() {
				t.Errorf("in use %v, round %d: every ping lost and %s was called better (%s)", inUse, round, l.better.Name, l.betterWhy)
			}
		}
	}
}

// The pings of the server in use alone (probeRound(false)) bring its line up to date but are not full rounds:
// a better server is ready by its answer time only after two full rounds.
func TestVRPickInUseRoundsAreNotFullRounds(t *testing.T) {
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 4, "192.0.2.2": 30, "192.0.2.3": 25}[ip] + float64(ping%3)*0.1, false
	})
	l := vrPicker(t, true)
	l.probeRound(true)
	l.probeRound(false)
	l.probeRound(false)
	if l.betterReady() {
		t.Errorf("ready to move after one full round and two rounds of the server in use alone")
	}
	l.probeRound(true)
	if !l.betterReady() {
		t.Errorf("not ready after two full rounds")
	}
}

// probeTick moves the VPN by itself only once nothing has played for probeIdle. The first round after a start
// and a round asked for from the page run whatever plays (pings cut nothing), and with no server chosen yet the
// best one is ready at once: neither may move the VPN while something plays. Once nothing has played for
// probeIdle, the same tick moves it.
func TestVRPickTickMovesOnlyAfterTheQuietSpell(t *testing.T) {
	oldWait := switchWait
	switchWait = 300 * time.Millisecond // (the stand-in VPN never comes up: the move is given up at once)
	defer func() { switchWait = oldWait }()
	vrSocket(t) // (the proxy part answers: nothing plays at this moment)
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}[ip], false
	})
	l := vrPicker(t, false)
	moved := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.probeSkip) > 0 || l.vpnKey != "" || l.pick.Env != ""
	}
	l.probeTick(0) // the first round after a start, with something playing
	if moved() {
		t.Fatalf("the first round after a start moved the VPN while something played")
	}
	l.mu.Lock()
	ready, pinged := l.betterReady(), !l.probeAt.IsZero()
	l.mu.Unlock()
	if !pinged || !ready {
		t.Fatalf("the first round did not run or found no first choice (pinged %v, ready %v)", pinged, ready)
	}
	l.mu.Lock()
	l.probeAsked = true
	l.mu.Unlock()
	l.probeTick(0) // the page's button, with something playing
	if moved() {
		t.Fatalf("a round asked for from the page moved the VPN by itself while something played")
	}
	l.probeTick(probeIdle) // nothing has played for probeIdle
	if !moved() {
		t.Errorf("control: nothing has played for %v, a first choice is ready, and the VPN was not moved", probeIdle)
	}
}

// switchServer looks again right before it moves the VPN: a recording that started after the tick's look keeps
// its VPN. The page's "switch now" goes on regardless (the page said what it costs).
func TestVRSwitchLooksAgainBeforeMoving(t *testing.T) {
	oldWait := switchWait
	switchWait = 300 * time.Millisecond
	defer func() { switchWait = oldWait }()
	play, _ := vrSocket(t)
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}[ip], false
	})
	l := vrPicker(t, false)
	l.probeRound(true)
	play() // a recording starts
	l.switchServer(false)
	if len(l.probeSkip) > 0 || l.vpnKey != "" {
		t.Fatalf("the VPN was moved by itself while a recording played")
	}
	l.switchServer(true)
	if len(l.probeSkip) == 0 {
		t.Errorf("control: \"switch now\" from the page did not move the VPN")
	}
}

// A provider that locks its account to one address (locked_to_address): a move changes the address and makes it
// wait, so the server in use is changed only from the page, never by itself, however much better another is.
func TestVRSwitchLeavesALockedAccountsServer(t *testing.T) {
	oldWait := switchWait
	switchWait = 300 * time.Millisecond
	defer func() { switchWait = oldWait }()
	vrSocket(t)
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 4, "192.0.2.2": 40, "192.0.2.3": 25}[ip], false
	})
	for _, locked := range []bool{true, false} {
		l := vrPicker(t, false)
		lock := map[bool]string{true: "true", false: "false"}[locked]
		l.set = &settings{}
		l.set.parse([]byte(`{"providers":[{"name":"One","url":"http://198.51.100.7","user":"u","password":"p","port":8101,"advanced":{"locked_to_address":` + lock + `}}],` +
			`"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
		l.pick = picked{Key: pickKey(l.set.vpnEnv()), Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
		l.probeRound(true)
		l.probeRound(true)
		if !l.betterReady() {
			t.Fatalf("locked %v: a server 36 ms closer two rounds running is not ready", locked)
		}
		l.switchServer(false)
		if moved := len(l.probeSkip) > 0; moved == locked {
			t.Errorf("locked to one address %v: moved by itself %v", locked, moved)
		}
		if locked {
			l.switchServer(true)
			if len(l.probeSkip) == 0 {
				t.Errorf("locked to one address: \"switch now\" from the page did not move the VPN")
			}
		}
	}
}
