package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every stop of the VPN nobody asked for (a server move, an update, an undo) looks at what plays right before it
// stops it, not only before a slow step: the registry asked through the VPN (seconds, up to fetchLimit) or a wait
// for another change holding the launcher. A recording or a viewer that starts during that step keeps its VPN.

// rcStopped: whether the VPN stand-in c was stopped.
func rcStopped(c *child) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// rcOnManifest runs do at each manifest the fake registry is asked for (a look-up through the VPN).
func rcOnManifest(f *fakeRegistry, do func()) {
	inner := f.Server.Config.Handler
	f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			do()
		}
		inner.ServeHTTP(w, r)
	})
}

// rcMover: the picker's launcher (three New York servers, none chosen yet, NY#2 the closest) on gluetun "latest"
// with a fake registry whose newest build is already kept here, the VPN up on a stand-in, and the registry last
// asked long ago: a start of the VPN asks it again first, through the VPN (askBeforeStop).
func rcMover(t *testing.T) (l *launcher, f *fakeRegistry, vpn *child) {
	f = newFakeRegistry(t)
	oldRegs, oldTry, oldWait := registries, tryProgram, switchWait
	t.Cleanup(func() { registries, tryProgram, switchWait = oldRegs, oldTry, oldWait })
	registries, tryProgram, switchWait = []registry{f.reg()}, func(string) error { return nil }, 300*time.Millisecond
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}[ip], false
	})
	l = vrPicker(t, false)
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"latest"}}`))
	vrBuild(t, f, binDir(l.dir), 1, time.Hour)
	vpn = vrRunningVPN(t, l, "")
	l.probeRound(true)
	if !l.betterReady() {
		t.Fatalf("no first choice of server is ready to move to")
	}
	return l, f, vpn
}

// A move to a better server by itself (probe.go): the tick saw nothing playing, then apply asked the registry
// through the VPN before stopping it (askBeforeStop) and stopped it without looking again. A recording that
// starts during that ask, or video that reaches a player then, keeps the VPN. With nothing playing the same
// move goes ahead.
func TestVRSwitchLooksAgainAfterAskingTheRegistry(t *testing.T) {
	play, _ := vrSocket(t)
	for _, c := range []struct {
		what  string
		start func()
	}{
		{"a recording starts", play},
		{"video reaches a player", func() { videoAt.Store(time.Now().UnixNano()) }},
	} {
		l, f, vpn := rcMover(t)
		rcOnManifest(f, c.start)
		l.switchServer(false)
		f.mu.Lock()
		asked := f.asked["manifest"]
		f.mu.Unlock()
		if asked == 0 {
			t.Fatalf("%s: the registry was not asked before the move (the case is not the one tested)", c.what)
		}
		if rcStopped(vpn) {
			t.Errorf("%s while the registry was asked through the VPN, and the VPN was stopped for a move nobody asked for", c.what)
		}
		l.mu.Lock()
		try, skip := l.pickTry, len(l.probeSkip)
		l.mu.Unlock()
		if try != "" || skip != 0 {
			t.Errorf("%s: the move given up is still being tried (%q) or a server was left out (%d)", c.what, try, skip)
		}
		mu.Lock()
		channels = map[string]*channel{}
		mu.Unlock()
		videoAt.Store(0)
	}
	// Control: nothing plays; the same move stops the VPN (and goes back, the stand-in does not connect).
	l, _, vpn := rcMover(t)
	l.switchServer(false)
	if !rcStopped(vpn) || len(l.probeSkip) == 0 {
		t.Errorf("control: with nothing playing the VPN was not moved to the better server")
	}
}

// rcUndoer: a launcher on "latest" whose VPN was started on a newer build it has never connected with here
// (published, then taken at a start), the build before it the one it last connected with (good.json). The VPN
// stand-in runs; the health check calls it down.
func rcUndoer(t *testing.T) (l *launcher, f *fakeRegistry, vpn *child, good, bad string) {
	f = newFakeRegistry(t)
	oldRegs, oldTry, oldWait := registries, tryProgram, updateWait
	t.Cleanup(func() { registries, tryProgram, updateWait = oldRegs, oldTry, oldWait })
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	l = &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	bin := binDir(l.dir)
	good = vrBuild(t, f, bin, 1, 3*time.Hour)
	if got := l.chooseProgram("latest").Digest; got != good {
		t.Fatalf("the first build was not taken: %s", short(got))
	}
	writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": good})
	bad = vrBuild(t, f, bin, 2, 0)
	l.progAsked = time.Time{}
	if got := l.chooseProgram("latest").Digest; got != bad {
		t.Fatalf("the newer build was not taken at the start: %s", short(got))
	}
	vpn = vrRunningVPN(t, l, bad)
	l.mu.Lock()
	l.vpnUp, vpn.ever = false, false
	l.mu.Unlock()
	t.Cleanup(func() { l.mu.Lock(); l.prog = builtinProgram; l.mu.Unlock() }) // (a watch loop left running has nothing to undo)
	return l, f, vpn, good, bad
}

// rcVideo keeps video reaching a player (every 2 s) until the test ends or stop is called.
func rcVideo(t *testing.T) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done); videoAt.Store(0) }) }
	t.Cleanup(stop)
	go func() {
		for {
			videoAt.Store(time.Now().UnixNano())
			select {
			case <-done:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return stop
}

// The VPN health check fails on a newer build while the tunnel carries every stream (a busy machine; the look,
// not the tunnel, is broken). watchProgram undid the build before it looked at whether video still went through:
// the VPN was started again under the viewer, and the build it works on was left alone for good (skip.json).
// While video flows nothing is undone; once video stops (a dead tunnel stops it within seconds), the build is.
// (About 65 s: the loop looks every 20 s.)
func TestVRUndoWaitsWhileVideoStillFlows(t *testing.T) {
	if testing.Short() {
		t.Skip("about 65 s")
	}
	vrSocket(t)
	l, _, vpn, good, bad := rcUndoer(t)
	updateWait = time.Second // (two minutes, shortened)
	bin := binDir(l.dir)
	stopVideo := rcVideo(t)
	go l.watchProgram()
	time.Sleep(46 * time.Second) // (the loop's first look, 20 s in, notes the VPN down; the second, 40 s in, would undo)
	if rcStopped(vpn) || skipped(bin) != "" {
		t.Fatalf("video went through the VPN the whole time and the build it ran on was undone (VPN stopped %v, left alone: %q)", rcStopped(vpn), short(skipped(bin)))
	}
	l.mu.Lock()
	run := l.prog.Digest
	l.mu.Unlock()
	if run != bad {
		t.Fatalf("the VPN program changed to %s while video went through", short(run))
	}
	// Control: video stops; at the loop's next look the build is undone.
	stopVideo()
	for end := time.Now().Add(30 * time.Second); !rcStopped(vpn) && time.Now().Before(end); {
		time.Sleep(250 * time.Millisecond)
	}
	if !rcStopped(vpn) || skipped(bin) != bad {
		t.Fatalf("control: no video and the VPN down on a build it never connected with, and it was not undone (stopped %v, left alone %q)", rcStopped(vpn), short(skipped(bin)))
	}
	l.mu.Lock()
	run = l.prog.Digest
	l.mu.Unlock()
	if run != good {
		t.Errorf("control: after the undo the VPN runs %s, want the build it last connected with (%s)", short(run), short(good))
	}
}

// undoBuild asks the registry through the VPN it is about to stop (which can take up to fetchLimit). If the VPN
// comes up meanwhile, or video goes through it again, it is left as it is and the build is not left alone. With
// the VPN still down and no video, the build is undone.
func TestVRUndoLooksAgainAfterAskingTheRegistry(t *testing.T) {
	vrSocket(t)
	for _, c := range []struct {
		what   string
		during func(l *launcher)
	}{
		{"the VPN comes up", func(l *launcher) { l.mu.Lock(); l.vpnUp = true; l.mu.Unlock() }},
		{"video goes through the VPN", func(*launcher) { videoAt.Store(time.Now().UnixNano()) }},
	} {
		l, f, vpn, _, bad := rcUndoer(t)
		rcOnManifest(f, func() { c.during(l) })
		if l.undoBuild(connected(binDir(l.dir)), updateWait+time.Second) {
			t.Errorf("%s while the registry was asked, and undoBuild says it undid the build", c.what)
		}
		if rcStopped(vpn) {
			t.Errorf("%s while the registry was asked, and the VPN was stopped to undo its build", c.what)
		}
		if s := skipped(binDir(l.dir)); s != "" {
			t.Errorf("%s: the build it runs on is left alone for good (%s)", c.what, short(s))
		}
		if l.prog.Digest != bad {
			t.Errorf("%s: the VPN program changed to %s", c.what, short(l.prog.Digest))
		}
		videoAt.Store(0)
	}
	// Control: still down, no video: undone, and the build before it runs.
	l, _, vpn, good, bad := rcUndoer(t)
	if !l.undoBuild(good, updateWait+time.Second) || !rcStopped(vpn) || skipped(binDir(l.dir)) != bad || l.prog.Digest != good {
		t.Errorf("control: the build the VPN never connected with was not undone (stopped %v, left alone %q, running %s)",
			rcStopped(vpn), short(skipped(binDir(l.dir))), short(l.prog.Digest))
	}
}

// An update installed by itself whose VPN did not come up within updateWait is undone (autoUpdate), and the
// registry is asked first. A VPN that connects on the new build just then (a slow connect) keeps it: stopping it
// would cut a VPN that works and leave a good build alone for good. (The undo of a build that never connects is
// TestVRUpdateThatConnectsSlowlyIsKeptOneThatNeverDoesIsUndone.)
func TestVRUpdateUndoLooksAgainAfterAskingTheRegistry(t *testing.T) {
	vrSocket(t)
	l, f, vpn, _, newer := vrUpdater(t)
	updateWait = 2 * time.Second
	var armed atomic.Bool
	var back atomic.Pointer[child]
	go func() { <-vpn.done; armed.Store(true) }() // (the update stopped the old one: the next look-up is the undo's)
	rcOnManifest(f, func() {
		if armed.Load() && back.Load() == nil {
			back.Store(vrRunningVPN(t, l, newer)) // the VPN connects on the new build
		}
	})
	l.autoUpdate()
	c := back.Load()
	if c == nil {
		t.Fatalf("the update was not undone after updateWait (the case is not the one tested)")
	}
	if rcStopped(c) {
		t.Errorf("the VPN came up on the new build while the undo asked the registry, and was stopped anyway")
	}
	if s := skipped(binDir(l.dir)); s != "" {
		t.Errorf("the build the VPN came up with is left alone for good (%s)", short(s))
	}
	if l.prog.Digest != newer {
		t.Errorf("the VPN program changed to %s, want the new build %s", short(l.prog.Digest), short(newer))
	}
}

// A stop nobody asked for can wait for the launcher first: another change holds it (here the proxy part, which
// had crashed, being started again: startProxy runs under l.work). A recording that comes back to that proxy part
// during the wait keeps the VPN: the look comes after the wait. With nothing playing after it, the update goes on.
func TestVRUpdateLooksAgainAfterWaitingForTheLauncher(t *testing.T) {
	play, _ := vrSocket(t)
	l, _, vpn, _, _ := vrUpdater(t)
	for _, playing := range []bool{true, false} {
		l.work.Lock()
		done := make(chan bool, 1)
		go func() { done <- l.applyIf(true, quietNow) }() // what an update installed by itself does once fetched
		time.Sleep(300 * time.Millisecond)
		if playing {
			play()
		}
		l.work.Unlock()
		went := <-done
		if playing && (went || rcStopped(vpn)) {
			t.Errorf("a recording came back while the update waited for the launcher, and the VPN was stopped for it")
		}
		if !playing && (!went || !rcStopped(vpn)) {
			t.Errorf("control: nothing played after the wait and the update did not stop the VPN (went on %v)", went)
		}
		mu.Lock()
		channels = map[string]*channel{}
		mu.Unlock()
	}
}

// A server chosen earlier that has not connected for 2 minutes is dropped (watchProgram, dropChoice), and the VPN
// started again. If video goes through it again while the restart waits for the launcher, the VPN is left as it is
// and the choice kept. With the VPN still down and no video, it is dropped.
func TestVRDropChoiceLooksAgainAfterWaitingForTheLauncher(t *testing.T) {
	vrSocket(t)
	for _, video := range []bool{true, false} {
		l := vrPicker(t, true)
		vpn := vrRunningVPN(t, l, "")
		os.MkdirAll(filepath.Dir(pickFile(l.dir)), 0o755)
		writeWhole(pickFile(l.dir), l.pick)
		l.mu.Lock()
		l.vpnKey += "picked " + l.pick.Env + "\n" // (as apply started it: on the server chosen)
		l.vpnUp = false                           // the health check calls it down
		l.mu.Unlock()
		l.work.Lock()
		go func() {
			time.Sleep(300 * time.Millisecond)
			if video {
				videoAt.Store(time.Now().UnixNano())
			}
			l.work.Unlock()
		}()
		dropped := l.dropChoice()
		_, err := os.Stat(pickFile(l.dir))
		l.mu.Lock()
		kept := l.pick.Env
		l.mu.Unlock()
		if video && (dropped || rcStopped(vpn) || kept != "SERVER_NAMES=NY#2" || err != nil) {
			t.Errorf("video went through the VPN again before it was stopped, and the choice was dropped (%v) or the VPN stopped (%v); kept %q, pick.json %v",
				dropped, rcStopped(vpn), kept, err)
		}
		if !video && (!dropped || !rcStopped(vpn) || kept != "" || err == nil) {
			t.Errorf("control: still down with no video, and the choice was not dropped (%v, stopped %v, kept %q, pick.json %v)", dropped, rcStopped(vpn), kept, err)
		}
		videoAt.Store(0)
	}
}
