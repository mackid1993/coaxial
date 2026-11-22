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

// Two newer builds in a row that the VPN never connects with (a regression in gluetun usually lasts more than
// one build): the second is undone like the first, and the VPN must go back to the build it last connected
// with, not to the first bad one, and not take turns between the two bad ones every few minutes.
func TestTwoBadBuildsInARow(t *testing.T) {
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
	// build: a build published under "latest", as kept after its download (age: when that was)
	build := func(n int, age time.Duration) string {
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
		l.progAsked = time.Time{}
		return d
	}
	good := build(1, 3*time.Hour)
	if l.chooseProgram("latest").Digest != good {
		t.Fatal("the first build was not taken")
	}
	writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": good}) // the VPN connected with it
	bad1 := build(2, time.Hour)
	if l.chooseProgram("latest").Digest != bad1 {
		t.Fatal("the second build was not taken")
	}
	if !l.undoBuild(good, updateWait+time.Second) || l.prog.Digest != good {
		t.Fatalf("the first bad build was not undone: running %s", short(l.prog.Digest))
	}
	bad2 := build(3, 0)
	if l.chooseProgram("latest").Digest != bad2 {
		t.Fatal("the third build was not taken")
	}
	if !l.undoBuild(good, updateWait+time.Second) {
		t.Fatal("the second bad build was not undone")
	}
	for i := 0; i < 3; i++ { // (each round: what watchProgram does after updateWait down)
		if run := l.prog.Digest; run == bad1 || run == bad2 {
			t.Fatalf("round %d: back on a build the VPN never connected with (%s; good %s, bad %s and %s)", i, short(run), short(good), short(bad1), short(bad2))
		}
		l.undoBuild(good, updateWait+time.Second)
	}
}
