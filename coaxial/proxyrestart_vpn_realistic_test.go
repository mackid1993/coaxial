package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The launcher starts the proxy part as "<this program> proxy". Here the test program stands in for it when the
// folder of its names file (COAXIAL_NAMES_FILE, which the launcher always hands it) holds a file "vr-proxy": it
// notes each start in "vr-proxy-starts", and its first start ends by itself after 300 ms when that file says
// "crash" (as a proxy part that crashed). Otherwise it runs until it is told to stop.
func init() {
	if len(os.Args) < 2 || os.Args[1] != "proxy" {
		return
	}
	dir := filepath.Dir(os.Getenv("COAXIAL_NAMES_FILE"))
	plan, err := os.ReadFile(filepath.Join(dir, "vr-proxy"))
	if err != nil {
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	starts, _ := os.ReadFile(filepath.Join(dir, "vr-proxy-starts"))
	n := strings.Count(string(starts), "\n") + 1
	f, _ := os.OpenFile(filepath.Join(dir, "vr-proxy-starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	fmt.Fprintf(f, "%d\n", time.Now().UnixMilli())
	f.Close()
	if n == 1 && strings.TrimSpace(string(plan)) == "crash" {
		time.Sleep(300 * time.Millisecond)
		os.Exit(2)
	}
	<-sig
	os.Exit(0)
}

// vrProxyLauncher: a launcher with its VPN up (a stand-in child), set to start the proxy part, which the test
// program stands in for (see init). starts: how many times the proxy part has been started.
func vrProxyLauncher(t *testing.T, plan string) (l *launcher, starts func() int) {
	dir := t.TempDir()
	if os.Getuid() == 0 { // the launcher starts the proxy part as nobody: it must reach this program and the folder
		self, err := os.Executable()
		if err != nil {
			t.Skip("the test program cannot be found")
		}
		for d := filepath.Dir(self); d != "/" && d != "."; d = filepath.Dir(d) {
			vrOpenDir(t, d, 0o011)
		}
		for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
			vrOpenDir(t, d, 0o011)
		}
		os.Chmod(dir, 0o777)
	}
	os.WriteFile(filepath.Join(dir, "vr-proxy"), []byte(plan+"\n"), 0o644)
	l = &launcher{dir: dir, quit: make(chan int, 4), prog: builtinProgram}
	l.vpn, l.vpnUp, l.vpnKey, l.proxyKey = &child{done: make(chan struct{}), ever: true}, true, "vpn settings\n", "{}"
	t.Cleanup(func() { l.mu.Lock(); l.vpnUp = false; l.mu.Unlock(); l.stopProxy() })
	return l, func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "vr-proxy-starts"))
		return strings.Count(string(data), "\n")
	}
}

// vrOpenDir adds bits to a folder's mode for the test's length (as root, the proxy part runs as nobody).
func vrOpenDir(t *testing.T, dir string, add os.FileMode) {
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm()&add == add {
		return
	}
	was := st.Mode().Perm()
	if os.Chmod(dir, was|add) == nil {
		t.Cleanup(func() { os.Chmod(dir, was) })
	}
}

func vrWaitStarts(starts func() int, n int, limit time.Duration) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if starts() >= n {
			return true
		}
	}
	return starts() >= n
}

// The proxy part crashes under viewers. The launcher starts it again by itself, about 2 s later, once, and
// leaves the VPN as it is: the same VPN program, the same settings, the same "came up at" (a provider locked to
// one address must not be made to wait as after a VPN change).
func TestVRProxyPartThatCrashedIsStartedAgain(t *testing.T) {
	l, starts := vrProxyLauncher(t, "crash")
	vpn, upAt := l.vpn, l.vpnUpAt
	l.work.Lock()
	l.startProxy()
	l.work.Unlock()
	if !vrWaitStarts(starts, 1, 10*time.Second) {
		t.Fatalf("the proxy part did not start")
	}
	if !vrWaitStarts(starts, 2, 6*time.Second) {
		t.Fatalf("the proxy part crashed and was not started again within 6 s")
	}
	time.Sleep(3 * time.Second)
	if n := starts(); n != 2 {
		t.Errorf("the proxy part crashed once and was started %d times", n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.vpn != vpn || !l.vpnUp || l.vpnKey != "vpn settings\n" || !l.vpnUpAt.Equal(upAt) {
		t.Errorf("the VPN was touched when the proxy part crashed: same program %v, up %v, settings %q", l.vpn == vpn, l.vpnUp, l.vpnKey)
	}
	if l.proxy == nil {
		t.Errorf("the proxy part started again is not the one the launcher keeps")
	}
}

// Stopped on purpose (a save, a VPN change): the proxy part stays stopped until the launcher starts it itself.
// One started again by its own crash rule would run beside the next one, or under a VPN being changed.
func TestVRProxyPartStoppedOnPurposeStaysStopped(t *testing.T) {
	l, starts := vrProxyLauncher(t, "run")
	l.work.Lock()
	l.startProxy()
	l.work.Unlock()
	if !vrWaitStarts(starts, 1, 10*time.Second) {
		t.Fatalf("the proxy part did not start")
	}
	time.Sleep(300 * time.Millisecond)
	l.stopProxy()
	time.Sleep(3500 * time.Millisecond)
	if n := starts(); n != 1 {
		t.Errorf("the proxy part was stopped on purpose and started again by itself (%d starts)", n)
	}
	l.mu.Lock()
	running := l.proxy != nil
	l.mu.Unlock()
	if running {
		t.Errorf("a proxy part runs after it was stopped on purpose")
	}
}

// The proxy part crashes while the looks at the VPN fail (a VPN that is down, or only a slow health check): it
// is not started again until the VPN is called up, and then at once (watchVPN calls startProxy after a good look).
func TestVRProxyPartWaitsForTheVPN(t *testing.T) {
	l, starts := vrProxyLauncher(t, "crash")
	l.work.Lock()
	l.startProxy()
	l.work.Unlock()
	if !vrWaitStarts(starts, 1, 10*time.Second) {
		t.Fatalf("the proxy part did not start")
	}
	l.mu.Lock()
	l.vpnUp = false
	l.mu.Unlock()
	time.Sleep(3500 * time.Millisecond)
	if n := starts(); n != 1 {
		t.Errorf("the proxy part was started again while the VPN was called down (%d starts)", n)
	}
	l.mu.Lock()
	l.vpnUp = true
	l.mu.Unlock()
	l.work.Lock()
	l.startProxy() // what watchVPN does after a good look
	l.work.Unlock()
	if !vrWaitStarts(starts, 2, 5*time.Second) {
		t.Errorf("the VPN is up again and the proxy part was not started")
	}
}
