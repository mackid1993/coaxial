package main

import (
	"bytes"
	"testing"
	"time"
)

// The shape of two freezes on a live system in one day: a player alone on a live HLS session keeps reloading
// its playlist, the session stands still at the provider, and the player does not start the channel over; it
// reloads the stopped playlist a few times and then asks for nothing at all, frozen until its app is closed.
// Straight at the provider nothing would save it either, but a new session moves on at once, so Coaxial opens
// one and the player's next reload of the same address is handed the moving playlist.
func TestHLSLoneReloaderOfAStalledPlaylistGetsANewSession(t *testing.T) {
	h, own, list := standstill(t) // (1 s pieces: the limit for a lone device is 1.5 s)
	const dev = "192.0.2.1"
	if st, _, err := devGet(h.base, dev, own, 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	for i := 0; i < 3; i++ { // it plays: the playlist moves
		devGet(h.base, dev, list, 10*time.Second)
		time.Sleep(200 * time.Millisecond)
	}
	h.stuck.Store(true)
	var moved bool
	for i := 0; i < 8 && !moved; i++ { // the player only reloads, every 0.5 s, for up to 4 s
		st, data, err := devGet(h.base, dev, list, 10*time.Second)
		if err != nil || st != 200 {
			t.Fatalf("reload %d: %d %v", i+1, st, err)
		}
		moved = !bytes.Contains(data, []byte("MEDIA-SEQUENCE:500"))
		time.Sleep(500 * time.Millisecond)
	}
	if !moved || h.opens.Load() != 2 {
		t.Errorf("a player alone on a stalled session kept reloading and was handed the stopped playlist every time (%d open(s) at the provider, want 2)", h.opens.Load())
	}
}

// With a second device on the session, one device's reloads do not take it from under the other before the
// longer limit (stoodStill).
func TestHLSReloadsOfAStalledSharedPlaylistKeepTheSession(t *testing.T) {
	h, own, list := standstill(t)
	for _, dev := range []string{"192.0.2.1", "192.0.2.2"} {
		if st, _, err := devGet(h.base, dev, own, 10*time.Second); err != nil || st != 200 {
			t.Fatalf("%s tunes: %d %v", dev, st, err)
		}
	}
	h.stuck.Store(true)
	for i := 0; i < 6; i++ {
		devGet(h.base, "192.0.2.1", list, 10*time.Second)
		devGet(h.base, "192.0.2.2", list, 10*time.Second)
		time.Sleep(500 * time.Millisecond)
	}
	if n := h.opens.Load(); n != 1 {
		t.Errorf("reloads with two devices on a stalled session opened the channel again (%d opens) before the longer limit", n)
	}
}
