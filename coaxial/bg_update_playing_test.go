package main

import (
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// playingSocket serves the proxy part's inside socket (what the launcher asks "is anything playing?") with
// no channel, film or HLS player, and gives back a way to put one reader on a channel.
func playingSocket(t *testing.T) (play func()) {
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
	keptVideo := videoAt.Swap(0) // (no video either: an earlier test's player may have had some moments ago)
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		mu.Unlock()
		videoAt.Store(keptVideo)
	})
	return func() {
		ch := &channel{key: "/ts bgtest 2", readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.8"}: true}}
		mu.Lock()
		channels[ch.key] = ch
		mu.Unlock()
	}
}

// Rule 1: nothing talks over the VPN on a timer while anything plays or records. The launcher's watch loop
// asked the VPN program's registry (a token and a manifest, through the tunnel) once an hour, and 20 s after
// every start, whatever was playing. A newer build is installed only after 10 minutes with nothing playing,
// so asking only then loses nothing. And not the moment the last stream ends either: only once nothing has
// played for a quiet spell. (Runs about two minutes: the loop looks every 20 s.)
func TestHourlyUpdateCheckWaitsWhilePlaying(t *testing.T) {
	if testing.Short() {
		t.Skip("about 70 s")
	}
	f := newFakeRegistry(t)
	oldRegs, oldTry := registries, tryProgram
	defer func() { registries, tryProgram = oldRegs, oldTry }()
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	oldQuiet := quietSpell
	quietSpell = 50 * time.Second // (five minutes, shortened)
	defer func() { quietSpell = oldQuiet }()
	play := playingSocket(t)
	play() // a recording runs
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	l.vpn, l.vpnUp = &child{ever: true}, true
	asked := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.asked["token"] + f.asked["manifest"]
	}
	go l.watchProgram()
	defer func() { l.mu.Lock(); l.vpn, l.vpnUp = nil, false; l.mu.Unlock() }() // (the loop then idles)
	time.Sleep(27 * time.Second)
	if n := asked(); n != 0 {
		t.Errorf("a recording runs and the registry was asked %d time(s) through the VPN by the hourly check", n)
	}
	// The recording ends. Nothing plays, but only just: a viewer may be on its way back. The check waits for a
	// quiet spell, where it used to ask at the loop's next look or the one after (20 to 40 s on).
	mu.Lock()
	channels = map[string]*channel{}
	mu.Unlock()
	time.Sleep(45 * time.Second)
	if n := asked(); n != 0 {
		t.Errorf("nothing has played for 45 s and the registry was asked %d time(s): the quiet spell (%v here) was not waited for", n, quietSpell)
	}
	// Control: once the quiet spell is over, the loop does ask.
	for end := time.Now().Add(45 * time.Second); asked() == 0 && time.Now().Before(end); {
		time.Sleep(250 * time.Millisecond)
	}
	if asked() == 0 {
		t.Errorf("control: with nothing playing for a quiet spell the hourly check never asked the registry")
	}
}

// An update installed by itself is meant to cut nothing (incident 11, rule 10). watchProgram saw nothing
// playing, then autoUpdate downloaded the new build through the VPN (seconds, up to two minutes) and stopped
// the VPN without looking again. A recording that started during the download was cut. pick.go's
// switchServer looks again right before it moves the VPN; the update must do the same.
func TestAutoUpdateLooksAgainBeforeStoppingTheVPN(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned image is for amd64")
	}
	f := newFakeRegistry(t)
	oldRegs, oldTry, oldWait := registries, tryProgram, updateWait
	defer func() { registries, tryProgram, updateWait = oldRegs, oldTry, oldWait }()
	registries, tryProgram, updateWait = []registry{f.reg()}, func(string) error { return nil }, 3*time.Second
	play := playingSocket(t)
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"player":{"user":"tv","password":"easy"},"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"single"}}`))
	old := l.chooseProgram("single").Digest
	// The VPN as it runs: a stand-in process that ends when the VPN is stopped.
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep program here")
	}
	defer cmd.Process.Kill()
	c := &child{cmd: cmd, done: make(chan struct{}), ever: true, prog: old}
	go func() { cmd.Wait(); close(c.done) }()
	_, key, _ := l.gluetunEnv(l.set)
	l.mu.Lock()
	l.vpn, l.vpnUp, l.vpnKey = c, true, key
	l.mu.Unlock()
	// A newer build is published and the hourly look found it.
	f.mu.Lock()
	f.tags["single"] = f.index
	f.mu.Unlock()
	l.checkProgram("hourly")
	if !nothingPlaying() {
		t.Fatal("something plays before the update")
	}
	// The DVR starts a recording while the new build downloads.
	inner := f.Server.Config.Handler
	f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") {
			play()
		}
		inner.ServeHTTP(w, r)
	})
	l.autoUpdate() // what watchProgram calls after 10 minutes with nothing playing
	select {
	case <-c.done:
		t.Errorf("the VPN was stopped for an unasked update although a recording had started during its download")
	default:
	}
}
