package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// An update that could not be fetched (here: its layer does not match its digest) is not tried again by
// itself: otherwise the VPN is started again after every 10 quiet minutes, all night.
func TestFailedUpdateNotRetriedByItself(t *testing.T) {
	f := newFakeRegistry(t)
	oldRegs, oldTry, oldWait := registries, tryProgram, updateWait
	defer func() { registries, tryProgram, updateWait = oldRegs, oldTry, oldWait }()
	registries, tryProgram, updateWait = []registry{f.reg()}, func(string) error { return nil }, 50*time.Millisecond
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned image is for amd64")
	}
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	f.set(f.layer)
	l.updDigest = f.index // (as the hourly look found it)
	l.autoUpdate()
	if l.prog.Digest == f.index {
		t.Fatalf("the spoiled build was installed")
	}
	if l.updFailed != f.index {
		t.Errorf("the update that could not be fetched is not noted: %q", l.updFailed)
	}
}

// The VPN does not connect with a newer build taken at a start (a container started again in the middle of
// an update, or after a new build came out): after updateWait it is left alone and the build before it runs.
func TestUnconnectedBuildUndone(t *testing.T) {
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
	good := l.chooseProgram("single").Digest // the build the VPN connected with
	f.mu.Lock()
	f.tags["single"] = f.index // a newer build comes out
	f.mu.Unlock()
	l.progAsked = time.Time{}
	if p := l.chooseProgram("single"); p.Digest != f.index || p.Digest == good {
		t.Fatalf("the newer build was not taken: %+v", p)
	}
	if l.undoBuild(good, updateWait/2) || l.undoBuild(f.index, 2*updateWait) || l.undoBuild("", 2*updateWait) {
		t.Fatalf("undone too early, or the build it connected with")
	}
	if !l.undoBuild(good, updateWait+time.Second) {
		t.Fatalf("not undone")
	}
	if skipped(binDir(l.dir)) != f.index || l.prog.Digest != good {
		t.Errorf("skipped %q, running %q; want %q left alone and %q running", skipped(binDir(l.dir)), l.prog.Digest, f.index, good)
	}
	if _, err := os.Stat(filepath.Join(binDir(l.dir), "skip.json")); err != nil {
		t.Error(err)
	}
}
