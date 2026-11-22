package main

import (
	"os/exec"
	"runtime"
	"testing"
)

// gluetun leaves its firewall (OUTPUT DROP) and its name server (127.0.0.1) in place when it stops, so the
// registry can be reached only while the VPN runs. An update, by itself or from the button, must get the newer
// build before the VPN is stopped, or the start that follows can only fall back to the build it had.
func TestUpdateFetchedWhileTheVPNRuns(t *testing.T) {
	f := newFakeRegistry(t)
	oldRegs, oldTry := registries, tryProgram
	defer func() { registries, tryProgram = oldRegs, oldTry }()
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned image is for amd64")
	}
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"single"}}`))
	old := l.chooseProgram("single").Digest // the build running now
	// The VPN runs (a stand-in program). Stopping it closes the way out, as gluetun's firewall does.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	c := &child{cmd: cmd, done: make(chan struct{}), ever: true, prog: old}
	go func() {
		cmd.Wait()
		close(c.done)
		l.mu.Lock()
		if l.vpn == c {
			l.vpn, l.vpnUp = nil, false
		}
		l.mu.Unlock()
	}()
	l.mu.Lock()
	l.vpn, l.vpnUp = c, true
	l.mu.Unlock()
	f.mu.Lock()
	f.tags["single"] = f.index // a newer build comes out
	f.open = func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.vpn != nil }
	f.mu.Unlock()
	l.restartForProgram("(update)")
	l.mu.Lock()
	running := l.prog.Digest
	l.mu.Unlock()
	if running != f.index {
		t.Errorf("after the update the VPN runs %s, want the newer %s (it ran %s)", short(running), short(f.index), short(old))
	}
}
