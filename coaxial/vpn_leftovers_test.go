package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test program stands in for gluetun when COAXIAL_FAKE_VPN names a folder: it starts a child the way gluetun
// starts openvpn (in a process group of its own), notes its number there, and crashes.
func init() {
	if dir := os.Getenv("COAXIAL_FAKE_VPN"); dir != "" {
		c := exec.Command("/bin/sleep", "300")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		c.Start()
		os.WriteFile(filepath.Join(dir, "child"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(300 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
}

// When gluetun crashes (or is killed) before the VPN came up, the VPN is started again 30 s later, with an
// openvpn of its own. The old openvpn must not run on beside it, nor be left as a zombie of the container's
// first process (this test takes that place for what it starts).
func TestVPNLeavesNothingBehind(t *testing.T) {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 36, 1, 0); e != 0 { // PR_SET_CHILD_SUBREAPER
		t.Skip("cannot stand in for the container's first process here")
	}
	dir := t.TempDir()
	old := builtinProgram
	builtinProgram.Path = os.Args[0]
	defer func() { builtinProgram = old }()
	l := &launcher{dir: dir, quit: make(chan int, 4), prog: builtinProgram}
	l.work.Lock()
	l.startVPN([]string{"COAXIAL_FAKE_VPN=" + dir}, nil, "builtin")
	l.work.Unlock()
	l.mu.Lock()
	c := l.vpn
	l.mu.Unlock()
	if c == nil {
		t.Fatal("the VPN program did not start")
	}
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the VPN program did not end")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "child"))
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal("the VPN program's child was not noted")
	}
	defer func() { syscall.Kill(pid, syscall.SIGKILL); syscall.Wait4(pid, nil, 0, nil) }()
	state := ""
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return // gone: stopped and collected
		}
		if f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:])); len(f) > 0 {
			state = f[0]
		}
	}
	t.Errorf("what the crashed VPN program started is still there (state %s: S running, Z a zombie)", state)
}
