package main

import (
	"testing"
	"time"
)

// A look at the VPN can fail with the VPN fine: the health check is a program started for each look, with 5 s to
// answer, and on a busy machine starting it takes longer than that, look after look. When the looks pass again
// and the tunnel's interface is the one from before, the tunnel was never gone and its address is the same, so
// the proxy part must not be told the VPN changed: a provider that locks the account to one address would
// refuse new streams for its whole wait, and a dropped channel would not be asked for again, with nothing
// changed. A tunnel made anew, an interface that is not known and a VPN program started anew are told as before.
func TestFailedLooksAtTheSameTunnelAreNotAChange(t *testing.T) {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	c := &child{done: make(chan struct{})}
	l.vpn, l.proxy = c, &child{done: make(chan struct{})}
	if !l.sawVPN(c, true, 7) {
		t.Fatalf("the VPN coming up was not noted")
	}
	long := time.Now().Add(-time.Hour)
	l.vpnUpAt = long
	if l.sawVPN(c, false, 0) || l.vpnUp {
		t.Fatalf("failed looks were not noted as down")
	}
	if l.sawVPN(c, true, 7) {
		t.Errorf("the looks failed and passed again on the same tunnel (interface 7): the proxy part was told the VPN changed")
	}
	if !l.vpnUp || !l.vpnUpAt.Equal(long) {
		t.Errorf("after failed looks at the same tunnel: up %v, came up at %v, want up and the time it really came up", l.vpnUp, l.vpnUpAt)
	}

	l.vpnUpAt = long
	if l.sawVPN(c, false, 0); !l.sawVPN(c, true, 8) || time.Since(l.vpnUpAt) > time.Minute {
		t.Errorf("down, and up again on a tunnel made anew (interface 8), was not told")
	}
	if l.sawVPN(c, false, 0); !l.sawVPN(c, true, 0) {
		t.Errorf("down and up again with the interface not known was not told")
	}
	l.sawVPN(c, true, 8)
	next := &child{done: make(chan struct{})} // (the VPN program started anew, as startVPN leaves it)
	l.vpn, l.vpnUp = next, false
	if !l.sawVPN(next, true, 8) {
		t.Errorf("a VPN program started anew came up and was not told")
	}
}
