package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gluetun's health check is a program the launcher starts for every look ("<program> healthcheck", 5 s to
// answer). On a busy machine starting it takes seconds, and some looks fail while the tunnel carries every
// stream. Here the test program stands in for it: run as "healthcheck" with COAXIAL_VR_HEALTH naming a folder,
// it takes the next line of that folder's plan ("<milliseconds> <exit code>"; the last line repeats), waits that
// long and leaves with that code. Each look is noted in the folder's "looks" file.
func init() {
	dir := os.Getenv("COAXIAL_VR_HEALTH")
	if dir == "" || len(os.Args) < 2 || os.Args[1] != "healthcheck" {
		return
	}
	data, _ := os.ReadFile(filepath.Join(dir, "looks"))
	n := strings.Count(string(data), "\n")
	plan, _ := os.ReadFile(filepath.Join(dir, "plan"))
	lines := strings.Split(strings.TrimSpace(string(plan)), "\n")
	var ms, code int
	fmt.Sscan(lines[min(n, len(lines)-1)], &ms, &code)
	f, _ := os.OpenFile(filepath.Join(dir, "looks"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintf(f, "%d\n", time.Now().UnixMilli())
	f.Close()
	time.Sleep(time.Duration(ms) * time.Millisecond)
	os.Exit(code)
}

// vrHealth: a launcher whose VPN stand-in is up from the start, and whose looks at it follow plan. watchVPN runs
// until the test ends. It returns how many looks have been taken so far.
func vrHealth(t *testing.T, plan ...string) (l *launcher, looks func() int) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("the test program cannot be found to stand in for the health check")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644)
	l = &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: program{Path: self, Source: "builtin"}}
	l.vpn, l.vpnEnv = &child{done: make(chan struct{})}, []string{"COAXIAL_VR_HEALTH=" + dir}
	go l.watchVPN()
	t.Cleanup(func() { l.mu.Lock(); l.vpn = nil; l.mu.Unlock() }) // (the loop then idles)
	return l, func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "looks"))
		return strings.Count(string(data), "\n")
	}
}

// vrWatchUp notes, every 20 ms, whether the launcher calls the VPN up, until stop is closed.
func vrWatchUp(l *launcher, stop chan struct{}) (downs func() []time.Duration) {
	var mu sync.Mutex
	var seen []time.Duration
	began := time.Now()
	go func() {
		wasUp := false
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			l.mu.Lock()
			up := l.vpnUp
			l.mu.Unlock()
			if wasUp && !up {
				mu.Lock()
				seen = append(seen, time.Since(began).Round(100*time.Millisecond))
				mu.Unlock()
			}
			wasUp = up
		}
	}()
	return func() []time.Duration { mu.Lock(); defer mu.Unlock(); return append([]time.Duration(nil), seen...) }
}

// A healthy tunnel on a busy machine: every look takes 3.5 to 4.6 s (inside the 5 s it is given) and one of them
// fails outright, as a health check program that could not start in time does. The launcher must keep calling
// the VPN up the whole time: a "down" here logs a VPN outage that never happened, makes the page and the
// container's own health check report the VPN down under running streams, and after two minutes of it a chosen
// server would be dropped and the VPN started again (watchProgram). And it must not note a new start of the
// tunnel (vpnUpAt): that tells a provider locked to one address to wait as after a real change. (About 40 s.)
func TestVRSlowLooksAtAHealthyTunnelKeepItUp(t *testing.T) {
	if testing.Short() {
		t.Skip("about 40 s")
	}
	old := lookAgain
	lookAgain = 2 * time.Second // (as shipped)
	defer func() { lookAgain = old }()
	l, looks := vrHealth(t, "3500 0", "4200 1", "3800 0", "4600 0")
	stop := make(chan struct{})
	defer close(stop)
	downs := vrWatchUp(l, stop)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		l.mu.Lock()
		up := l.vpnUp
		l.mu.Unlock()
		if up {
			break
		}
	}
	l.mu.Lock()
	up, cameUp := l.vpnUp, l.vpnUpAt
	l.mu.Unlock()
	if !up {
		t.Fatalf("a look that passed in 3.5 s did not bring the VPN up (looks taken: %d)", looks())
	}
	for end := time.Now().Add(45 * time.Second); looks() < 4 && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(5 * time.Second) // (the fourth look takes 4.6 s)
	if n := looks(); n < 4 {
		t.Fatalf("only %d looks were taken in 45 s", n)
	}
	if d := downs(); len(d) > 0 {
		t.Errorf("the tunnel was fine and the looks only slow (one failed): the VPN was called down at %v", d)
	}
	l.mu.Lock()
	up, again := l.vpnUp, l.vpnUpAt
	l.mu.Unlock()
	if !up {
		t.Errorf("after slow looks at a healthy tunnel the VPN is called down")
	}
	if !again.Equal(cameUp) {
		t.Errorf("slow looks at the same tunnel were noted as a new start of the VPN (%v, then %v)", cameUp, again)
	}
}

// The other direction: the tunnel is really gone and gluetun's health check hangs (its own server does not
// answer). Each look is cut off at its 5 s, the second look is taken, and the VPN is called down within one
// loop: up looks come every 10 s, so down by about 10 + 5 + 2 + 5 s. A look left to hang would hold the loop,
// and the launcher would go on calling a dead VPN up. (About 25 s.)
func TestVRHungLooksAtADeadTunnelCallItDown(t *testing.T) {
	if testing.Short() {
		t.Skip("about 25 s")
	}
	old := lookAgain
	lookAgain = 2 * time.Second
	defer func() { lookAgain = old }()
	l, looks := vrHealth(t, "200 0", "60000 0")
	var upAt time.Time
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		l.mu.Lock()
		up := l.vpnUp
		l.mu.Unlock()
		if up {
			upAt = time.Now()
			break
		}
	}
	if upAt.IsZero() {
		t.Fatalf("a quick good look did not bring the VPN up")
	}
	var downAt time.Time
	for end := time.Now().Add(40 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		l.mu.Lock()
		up := l.vpnUp
		l.mu.Unlock()
		if !up {
			downAt = time.Now()
			break
		}
	}
	if downAt.IsZero() {
		t.Fatalf("the health check hung look after look for 40 s and the VPN was still called up (%d looks)", looks())
	}
	if took := downAt.Sub(upAt); took > 26*time.Second {
		t.Errorf("the VPN was called down %v after its health check began to hang; want within about 22 s (one wait of 10 s, two looks of 5 s and 2 s between)", took.Round(100*time.Millisecond))
	}
	if n := looks(); n < 3 {
		t.Errorf("down after %d looks; want the good one, then two that hung", n)
	}
}

// A server the picker chose that no longer connects (taken out of service, or unreachable from here): after two
// minutes of the VPN down on it, the choice is dropped and gluetun picks for itself again (watchProgram). Not
// sooner: a slow reconnect on a lossy line takes well over a minute, and dropping the choice then starts the
// VPN again on a random server for nothing. (About 2 min 45 s: the loop looks every 20 s.)
func TestVRChosenServerDroppedOnlyAfterTwoMinutesDown(t *testing.T) {
	if testing.Short() {
		t.Skip("about 2 min 45 s")
	}
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	fakeServers(t, l)
	key := pickKey(l.set.vpnEnv())
	l.pick = picked{Key: key, Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
	os.MkdirAll(filepath.Dir(pickFile(l.dir)), 0o755)
	writeWhole(pickFile(l.dir), l.pick)
	chosen := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.pick.Env != ""
	}
	began := time.Now()
	go l.watchProgram() // the VPN is down from the start (l.vpnUp false)
	for time.Since(began) < 115*time.Second {
		if !chosen() {
			t.Fatalf("the chosen server was dropped after %v with the VPN down; want only after 2 minutes", time.Since(began).Round(time.Second))
		}
		time.Sleep(250 * time.Millisecond)
	}
	for end := began.Add(170 * time.Second); chosen() && time.Now().Before(end); {
		time.Sleep(250 * time.Millisecond)
	}
	if chosen() {
		t.Fatalf("the VPN has been down on the chosen server for well over 2 minutes and the choice was kept")
	}
	if _, err := os.Stat(pickFile(l.dir)); err == nil {
		t.Errorf("the choice was dropped but pick.json is still there (a restart would take it again)")
	}
}

// The same two minutes of failed health checks while video still goes through the tunnel (the looks fail, the
// tunnel works: a busy machine). The choice is kept and nothing is started again: that would cut what plays.
func TestVRChosenServerKeptWhileVideoStillFlows(t *testing.T) {
	if testing.Short() {
		t.Skip("about 3 min")
	}
	playingSocket(t)
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	fakeServers(t, l)
	key := pickKey(l.set.vpnEnv())
	l.pick = picked{Key: key, Env: "SERVER_NAMES=NY#2", Name: "NY#2"}
	os.MkdirAll(filepath.Dir(pickFile(l.dir)), 0o755)
	writeWhole(pickFile(l.dir), l.pick)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // video keeps reaching a player
		for {
			videoAt.Store(time.Now().UnixNano())
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	began := time.Now()
	go l.watchProgram() // the health check calls the VPN down from the start (l.vpnUp false)
	for time.Since(began) < 170*time.Second {
		l.mu.Lock()
		kept := l.pick.Env != ""
		l.mu.Unlock()
		if !kept {
			t.Fatalf("the chosen server was dropped after %v while video still went through the tunnel", time.Since(began).Round(time.Second))
		}
		time.Sleep(250 * time.Millisecond)
	}
	videoAt.Store(0)
}
