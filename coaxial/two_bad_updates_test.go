package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Two newer builds in a row that the VPN never connects with, taken the way an update takes them ("Update now",
// or by itself after 10 quiet minutes: restartForProgram). The cache was tidied only when a start asked the
// registry, so the first bad build stayed; once the second was undone, the VPN went back to the first bad one
// (the most recently used), then to the second, in turns, while the log said "going back to" the good one.
func TestTwoBadBuildsByUpdate(t *testing.T) {
	f := newFakeRegistry(t)
	oldRegs, oldTry := registries, tryProgram
	defer func() { registries, tryProgram = oldRegs, oldTry }()
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned programs are for amd64")
	}
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.set = &settings{}
	l.set.parse([]byte(`{"vpn":{"service_provider":"custom","type":"wireguard","gluetun_version":"latest"}}`))
	bin := binDir(l.dir)
	build := func(n int, age time.Duration) string { // a build published under "latest", as kept after its download
		m := []byte(fmt.Sprintf(`{"build":%d}`, n))
		d := "sha256:" + sumHex(m)
		dir := filepath.Join(bin, strings.TrimPrefix(d, "sha256:"))
		os.MkdirAll(dir, 0o755)
		file := filepath.Join(dir, programName)
		os.WriteFile(file, fakeProgram(2, 62, minProgram+100+n), 0o755)
		sum, err := checkProgramFile(file, "amd64")
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
	good := build(1, 3*time.Hour)
	if l.chooseProgram("latest").Digest != good {
		t.Fatal("the first build was not taken")
	}
	writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": good}) // the VPN connected with it
	for i, age := range []time.Duration{time.Hour, 0} {
		bad := build(i+2, age) // published; then taken by an update
		l.restartForProgram("(update)")
		if l.prog.Digest != bad {
			t.Fatalf("bad build %d was not taken by the update: running %s, want %s (good %s)", i+1, short(l.prog.Digest), short(bad), short(good))
		}
		if !l.undoBuild(good, updateWait+time.Second) {
			t.Fatalf("bad build %d was not undone", i+1)
		}
		if l.prog.Digest != good {
			t.Fatalf("after bad build %d was undone, the VPN runs %s, not the build it last connected with (%s)", i+1, short(l.prog.Digest), short(good))
		}
	}
}
