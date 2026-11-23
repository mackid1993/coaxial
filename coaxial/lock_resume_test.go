package main

import (
	"strings"
	"testing"
)

// With the account locked to one address, new streams wait after the VPN changed address. A film the change
// broke is not a new stream: its device asking for it again to go on takes the place of its own old session,
// as a live channel's reconnect does. Another film, or another device, still waits.
func TestLockLetsABrokenFilmResume(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), Locked: true})
	mu.Lock()
	was := vpnChanged
	vpnChanged = mono() - 1000
	mu.Unlock()
	defer func() { mu.Lock(); vpnChanged = was; mu.Unlock() }()
	a := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv")
	if a.StatusCode != 200 {
		t.Fatalf("the film before the change: %d", a.StatusCode)
	}
	a.Body.Close() // (the VPN went down under it)
	eventually(t, func() bool { return fp.count(fp.open, "") == 0 }, "the film breaking off")
	mu.Lock()
	vpnChanged = mono() // (and came back up somewhere else)
	mu.Unlock()
	again := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv", "Range", "bytes=5000000-")
	if again.StatusCode != 200 {
		t.Errorf("the device resuming the film the change broke: %d %q", again.StatusCode, body(again))
	} else {
		again.Body.Close()
	}
	if r := ask(t, base, "192.0.2.1", "/movie/tv/easy/2.mkv"); r.StatusCode != 503 || !strings.Contains(body(r), "locks your account") {
		t.Errorf("another film from the same device did not wait: %d", r.StatusCode)
	}
	if r := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv"); r.StatusCode != 503 {
		t.Errorf("another device on the same film did not wait: %d", r.StatusCode)
		r.Body.Close()
	}
}
