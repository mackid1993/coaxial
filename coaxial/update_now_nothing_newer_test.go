package main

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"testing"
)

// "Update now" when the newer build cannot be had (here every download of it fails its checksum): the VPN must
// be left running and the page told so. It was started again anyway, cutting what played for the same build,
// and the page said "The VPN client was updated".
func TestUpdateNowThatGetsNothingLeavesTheVPN(t *testing.T) {
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
	cmd := exec.Command("sleep", "60") // the VPN that runs (a stand-in)
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	defer cmd.Process.Kill()
	c := &child{cmd: cmd, done: make(chan struct{}), ever: true, prog: old}
	go func() { cmd.Wait(); close(c.done) }()
	l.mu.Lock()
	l.vpn, l.vpnUp = c, true
	l.mu.Unlock()
	f.mu.Lock()
	f.tags["single"] = f.index // a newer build is named...
	f.mu.Unlock()
	f.set(f.layer) // ...and its program never arrives whole
	rec := httptest.NewRecorder()
	l.apiProgramUpdate(rec, httptest.NewRequest("POST", "/api/vpn/program/update", nil))
	var answer struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	json.Unmarshal(rec.Body.Bytes(), &answer)
	l.mu.Lock()
	same, stopped, running := l.vpn == c, c.stopped, l.prog.Digest
	l.mu.Unlock()
	if answer.OK || answer.Message == "" {
		t.Errorf("the page was told %s", rec.Body)
	}
	if !same || stopped || running != old {
		t.Errorf("the VPN was started again for nothing (stopped %v, runs %s, ran %s)", stopped, short(running), short(old))
	}
}
