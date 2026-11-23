package main

import (
	"testing"
	"time"
)

// gluetun reconnects by itself when its own health check fails, and is unhealthy only for the few seconds that
// takes: the launcher, looking every 10 s, mostly never sees it down. The tunnel's interface is made anew at
// each connection, so a new interface index tells of the reconnect (and its address, perhaps new) all the same.
func TestVPNReconnectSeenByItsInterface(t *testing.T) {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram}
	c := &child{done: make(chan struct{})}
	l.vpn, l.proxy = c, &child{done: make(chan struct{})}
	if !l.sawVPN(c, true, 7) || l.vpnUpAt.IsZero() || !c.ever {
		t.Fatalf("the VPN coming up was not noted: at %v, ever %v", l.vpnUpAt, c.ever)
	}
	long := time.Now().Add(-time.Hour)
	l.vpnUpAt = long
	if l.sawVPN(c, true, 7) || !l.vpnUpAt.Equal(long) {
		t.Errorf("the same tunnel, looked at again, was taken for a reconnect")
	}
	if !l.sawVPN(c, true, 9) || time.Since(l.vpnUpAt) > time.Minute {
		t.Errorf("a reconnect between two looks (a new interface) was missed: the VPN came up at %v", l.vpnUpAt)
	}
	l.vpnUpAt = long
	if l.sawVPN(c, true, 0) || l.sawVPN(c, true, 0) || l.sawVPN(c, true, 9) || !l.vpnUpAt.Equal(long) {
		t.Errorf("with the interface not known, something was taken for a reconnect")
	}
	if l.sawVPN(&child{done: make(chan struct{})}, true, 11) || !l.vpnUpAt.Equal(long) {
		t.Errorf("a look at a VPN program no longer running counted")
	}
	if l.sawVPN(c, false, 0) || l.vpnUp || !l.sawVPN(c, true, 12) {
		t.Errorf("down and up again was not noted")
	}
}
