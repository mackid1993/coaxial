package main

import (
	"runtime"
	"testing"
)

// "Update now" takes the newer build. The newest build the hourly look had found (the one that was running)
// was kept: the page then offered the old build as "a newer VPN client is out", and once nothing had played for
// 10 minutes the VPN was started again for it (it reconnected on the same build: a needless cut).
func TestUpdateNowForgetsTheOlderNewest(t *testing.T) {
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
	old := l.chooseProgram("single").Digest
	l.checkProgram("hourly") // the registry names the build running: nothing newer
	f.mu.Lock()
	f.tags["single"] = f.index // a newer build comes out; "Update now" is pressed
	f.mu.Unlock()
	l.restartForProgram("(update)")
	l.mu.Lock()
	running := l.prog.Digest
	pending := l.updDigest != "" && l.updDigest != l.prog.Digest && l.updDigest != l.updFailed // (as watchProgram reads it)
	l.mu.Unlock()
	if running != f.index || running == old {
		t.Fatalf("the newer build was not taken: running %s", short(running))
	}
	if l.programUpdate()["available"] == true || pending {
		t.Errorf("after the update, %v is still offered as newer than %s (auto-update pending: %v)", l.programUpdate()["newest"], short(running), pending)
	}
}
