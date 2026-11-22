package main

import (
	"os/exec"
	"runtime"
	"testing"
)

// gluetun_version changed in the settings (to pin a build, say, after a bad "latest"): the VPN is started again,
// and the start asked the registry only after the VPN had stopped, when gluetun's leftover firewall and name
// server shut it out. The new tag was never fetched (until the container was restarted): the build it had ran on.
func TestNewTagFetchedWhileTheVPNRuns(t *testing.T) {
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
	_, key, _ := l.gluetunEnv(l.set)
	cmd := exec.Command("sleep", "60") // the VPN runs (a stand-in); stopping it closes the way out
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
	l.vpn, l.vpnUp, l.vpnKey = c, true, key
	l.mu.Unlock()
	f.mu.Lock()
	f.open = func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.vpn != nil }
	f.mu.Unlock()
	s := &settings{}
	s.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	l.mu.Lock()
	l.set = s // saved from the page
	l.mu.Unlock()
	l.apply()
	l.mu.Lock()
	running := l.prog.Digest
	l.mu.Unlock()
	if running != f.index {
		t.Errorf("after gluetun_version became latest the VPN runs %s, want latest's %s (it ran %s)", short(running), short(f.index), short(old))
	}
}
