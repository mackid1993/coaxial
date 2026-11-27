package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// vrBuild: a build of gluetun published under "latest" for this machine's processor, as kept after its
// download (age: how long ago that was). The fake registry then names it for "latest".
func vrBuild(t *testing.T, f *fakeRegistry, bin string, n int, age time.Duration) string {
	t.Helper()
	m := []byte(fmt.Sprintf(`{"vrbuild":%d}`, n))
	d := "sha256:" + sumHex(m)
	dir := filepath.Join(bin, strings.TrimPrefix(d, "sha256:"))
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, programName)
	kind, known := elfMachine[runtime.GOARCH]
	if !known {
		t.Skip("no ELF machine number known for " + runtime.GOARCH)
	}
	os.WriteFile(file, fakeProgram(byte(kind[0]), kind[1], minProgram+200+n), 0o755)
	sum, err := checkProgramFile(file, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	writeWhole(filepath.Join(dir, "info.json"), program{Tag: "latest", Version: fmt.Sprint(n), Source: "downloaded", Digest: d, Sum: sum})
	at := time.Now().Add(-age)
	os.Chtimes(dir, at, at)
	f.mu.Lock()
	f.manifests[d], f.tags["latest"] = m, d
	f.mu.Unlock()
	return d
}

// vrRunningVPN: a stand-in for the VPN program as it runs (a process that ends when the VPN is stopped), up and
// connected, on build digest. When it ends, the launcher forgets it, as startVPN's own watcher does.
func vrRunningVPN(t *testing.T, l *launcher, digest string) *child {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep program here")
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	c := &child{cmd: cmd, done: make(chan struct{}), ever: true, prog: digest}
	go func() {
		cmd.Wait()
		close(c.done)
		l.mu.Lock()
		if l.vpn == c {
			l.vpn, l.vpnUp = nil, false
		}
		l.mu.Unlock()
	}()
	_, key, _ := l.gluetunEnv(l.set)
	l.mu.Lock()
	l.vpn, l.vpnUp, l.vpnKey = c, true, key
	l.mu.Unlock()
	return c
}

// vrUpdater: a launcher on "latest" with a fake registry, the VPN running and connected on a first build (the
// one it last connected with, good.json), and a newer build published and found by the hourly look.
func vrUpdater(t *testing.T) (l *launcher, f *fakeRegistry, vpn *child, old, newer string) {
	f = newFakeRegistry(t)
	oldRegs, oldTry, oldWait := registries, tryProgram, updateWait
	t.Cleanup(func() { registries, tryProgram, updateWait = oldRegs, oldTry, oldWait })
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	l = &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	bin := binDir(l.dir)
	old = vrBuild(t, f, bin, 1, 3*time.Hour)
	if got := l.chooseProgram("latest").Digest; got != old {
		t.Fatalf("the first build was not taken: %s", short(got))
	}
	writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": old})
	vpn = vrRunningVPN(t, l, old)
	newer = vrBuild(t, f, bin, 2, 0)
	l.checkProgram("hourly")
	if l.updDigest != newer {
		t.Fatalf("the hourly look did not find the newer build")
	}
	return l, f, vpn, old, newer
}

// A newer build that connects slowly, as on a slow or busy VPN server: up 4 s after the VPN was started again,
// inside updateWait (7 s here, 2 minutes as shipped). It must be kept, not undone: undoing it starts the VPN
// again for nothing and leaves a good build alone for good. Then a build that never connects is undone after
// updateWait: the VPN goes back to the last one it connected with, and that build is left alone (skip.json).
func TestVRUpdateThatConnectsSlowlyIsKeptOneThatNeverDoesIsUndone(t *testing.T) {
	if testing.Short() {
		t.Skip("about 20 s")
	}
	vrSocket(t) // (the proxy part answers: nothing plays)
	l, f, vpn, _, newer := vrUpdater(t)
	updateWait = 7 * time.Second
	bin := binDir(l.dir)
	go func() { // the new build connects 4 s after the old one was stopped
		<-vpn.done
		time.Sleep(4 * time.Second)
		vrRunningVPN(t, l, newer)
	}()
	l.autoUpdate()
	if s := skipped(bin); s != "" {
		t.Fatalf("a build that connected after 4 s (of %v) was undone and left alone: %s", updateWait, short(s))
	}
	if l.prog.Digest != newer {
		t.Fatalf("the VPN runs %s after the update, not the newer build %s", short(l.prog.Digest), short(newer))
	}
	writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": newer}) // (what watchProgram notes once it is up)

	l.mu.Lock()
	vpn = l.vpn
	l.mu.Unlock()
	bad := vrBuild(t, f, bin, 3, 0) // published next; the VPN never connects with it
	l.checkProgram("hourly")
	began := time.Now()
	l.autoUpdate()
	if took := time.Since(began); took < updateWait {
		t.Errorf("the build that never connected was given up on after %v, before updateWait (%v)", took.Round(100*time.Millisecond), updateWait)
	}
	if s := skipped(bin); s != bad {
		t.Errorf("the build that never connected is not left alone: skip.json names %q, want %s", short(s), short(bad))
	}
	if l.prog.Digest != newer {
		t.Errorf("after the undo the VPN runs %s, want the build it last connected with (%s)", short(l.prog.Digest), short(newer))
	}
	select {
	case <-vpn.done:
	default:
		t.Errorf("the update never stopped the VPN to take the build")
	}
}

// The look right before an unasked update stops the VPN (restartIf, like switchServer's) must not take a proxy
// part that cannot answer for one that says nothing plays. Here a recording starts while the build downloads and
// the proxy part has stopped and is being started again (it is started 2 s after a crash; its viewers come back
// to it). Nothing can be known to be quiet then: the update waits for the next quiet spell. watchProgram's own
// look already counts a proxy part that does not answer as something playing.
func TestVRUpdateWaitsWhileTheProxyPartDoesNotAnswer(t *testing.T) {
	play, crash := vrSocket(t)
	l, f, vpn, old, _ := vrUpdater(t)
	updateWait = 2 * time.Second
	if !nothingPlaying() {
		t.Fatal("something plays before the update")
	}
	inner := f.Server.Config.Handler
	f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			play()  // the DVR starts a recording
			crash() // and the proxy part is not answering while it is started again
		}
		inner.ServeHTTP(w, r)
	})
	l.autoUpdate()
	select {
	case <-vpn.done:
		t.Errorf("the VPN was stopped for an unasked update while the proxy part could not say whether anything played")
	default:
	}
	if l.prog.Digest != old {
		t.Errorf("the VPN program changed to %s", short(l.prog.Digest))
	}
}

// The same look before a move to a better VPN server.
func TestVRSwitchWaitsWhileTheProxyPartDoesNotAnswer(t *testing.T) {
	oldWait := switchWait
	switchWait = 300 * time.Millisecond
	defer func() { switchWait = oldWait }()
	_, crash := vrSocket(t)
	vrPings(t, func(ip string, _, ping int) (float64, bool) {
		return map[string]float64{"192.0.2.1": 6, "192.0.2.2": 5, "192.0.2.3": 7}[ip], false
	})
	l := vrPicker(t, false)
	l.probeRound(true)
	crash()
	l.switchServer(false)
	if len(l.probeSkip) > 0 || l.vpnKey != "" {
		t.Errorf("the VPN was moved by itself while the proxy part could not say whether anything played")
	}
}

// What counts as playing, for every rule that waits until nothing plays (pings, update checks, updates, server
// moves), measured against real players. An HLS player asks for a piece every target duration, and at a provider
// pause of 15 to 17 s it asks for none until the next one is out: it still plays. A viewer who paused a film for
// 50 s still watches it. Each is counted, through the launcher's own question to the proxy part; a player gone
// past those limits is not.
func TestVRPlayingCountsRealPauses(t *testing.T) {
	vrSocket(t)
	now := mono()
	for _, gap := range []float64{4.9, 6.7, 8, 11, 13.3, 15.5, 17, 25} { // seconds since its last piece
		mu.Lock()
		hlsRows = map[string]*hlsRow{"vr": {first: now - 600, last: now - gap, who: "192.0.2.21", id: "7", port: "8101"}}
		mu.Unlock()
		if nothingPlaying() {
			t.Errorf("an HLS player whose last piece was %.1f s ago (a provider pause) was taken for nothing playing", gap)
		}
	}
	mu.Lock()
	hlsRows = map[string]*hlsRow{"vr": {first: now - 600, last: now - 45, who: "192.0.2.21", id: "7", port: "8101"}}
	mu.Unlock()
	if !nothingPlaying() {
		t.Errorf("an HLS player gone for 45 s still counts as playing")
	}
	mu.Lock()
	hlsRows = map[string]*hlsRow{}
	mu.Unlock()
	for _, paused := range []float64{5, 30, 50} {
		mu.Lock()
		films = map[string]*film{"192.0.2.22": {last: mono() - paused}}
		mu.Unlock()
		if nothingPlaying() {
			t.Errorf("a film paused %.0f s ago was taken for nothing playing", paused)
		}
	}
	mu.Lock()
	films = map[string]*film{"192.0.2.22": {last: mono() - 75}}
	mu.Unlock()
	if !nothingPlaying() {
		t.Errorf("a film whose last part was sent 75 s ago still counts as playing")
	}
}
