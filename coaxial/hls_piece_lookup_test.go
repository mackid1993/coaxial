package main

import (
	"net"
	"testing"
	"time"
)

// A piece of a live channel is asked for every few seconds over a connection that is already open. Once a minute
// the name of its server is looked up again before the piece is passed on. That look-up must not hold the piece:
// with a resolver that says nothing for a moment (a home router's, or the VPN's own while it restarts) the piece
// waited for the resolver, up to 10 s, which is longer than a player waits for a piece (ExoPlayer: 8 s). The
// playlist's reload has the same rule (TestHLSReloadWhenLookupFails).
func TestHLSPieceWhenLookupGetsNoAnswer(t *testing.T) {
	h, fr, _ := namedHLS(t)
	mu.Lock() // (what an earlier test learned about the name is not this test's business)
	delete(led, "video.example")
	delete(checks, "video.example")
	mu.Unlock()
	st, list, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second)
	if err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	piece := pieceOf(t, list)
	if st, data, err := devGet(h.base, "192.0.2.1", piece, 8*time.Second); err != nil || st != 200 || len(data) == 0 {
		t.Fatalf("the first piece: %d, %d bytes, %v", st, len(data), err)
	}
	aMinuteLater() // the name's last answer is over a minute old
	fr.mode.Store(2)
	began := time.Now()
	st, data, err := devGet(h.base, "192.0.2.1", piece, 8*time.Second) // (a player's own wait)
	took := time.Since(began)
	fr.mode.Store(0)
	t.Logf("the piece while the resolver said nothing: status %d, %d bytes, %v, after %.1f s", st, len(data), err, took.Seconds())
	if err != nil || st != 200 || len(data) == 0 {
		t.Errorf("the piece while the resolver said nothing: status %d, %d bytes, %v, want the piece", st, len(data), err)
	}
	if took > 4*time.Second {
		t.Errorf("the piece took %.1f s; its server's connection was open and the name's last answer in hand", took.Seconds())
	}
}

// What the look-up is for still holds on the piece path: a name whose last answer was "home" stays refused while
// the resolver says nothing, and a name that comes to lead home is refused once it is looked up again.
func TestHLSPieceNameStaysRefused(t *testing.T) {
	fr := newFakeResolver(t, net.ParseIP("127.0.0.1"))
	if nameOK(t.Context(), "home.example") {
		t.Fatalf("a name that leads to this machine was let through")
	}
	aMinuteLater()
	fr.mode.Store(2)
	began := time.Now()
	if nameOK(t.Context(), "home.example") {
		t.Errorf("a name that led home was let through while the resolver said nothing")
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("the check took %.1f s with the last answer in hand", took.Seconds())
	}
	fr.mode.Store(0)
	fr.lead.Store(net.ParseIP("198.51.100.7").To4())
	aMinuteLater()
	if !nameOK(t.Context(), "home.example") {
		t.Errorf("a name that leads elsewhere now is still refused")
	}
	fr.lead.Store(net.ParseIP("127.0.0.1").To4())
	aMinuteLater()
	if nameOK(t.Context(), "home.example") {
		t.Errorf("a name that came to lead home was let through")
	}
}
