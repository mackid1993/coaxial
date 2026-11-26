package main

import (
	"bytes"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A live HLS channel goes down at the provider for a while (its video server answers 404 for the playlist, or
// hangs up, while the provider restarts the channel), and the provider's own address still takes every open and
// sends it on. The design since the first version: a reload that fails opens the channel again once and tries
// the new session once, because the usual reason for a failed reload is a session the provider ended, and one
// new open cures that on the spot. So while a channel is really down, each ask by the player costs exactly one
// open at the provider, and a player gives up after a few asks (the reference players retry a failed playlist
// two or three times, about a second apart). What this holds the proxy to: never more than one open for one
// ask, nothing opened that no player asked for, and the picture back at the first reload once the channel is.
func TestHLSChannelDownOneOpenForEachAsk(t *testing.T) {
	for _, cut := range []bool{false, true} { // the video server answers 404, or hangs up without an answer
		t.Run(map[bool]string{false: "answers 404", true: "hangs up"}[cut], func(t *testing.T) { channelDown(t, cut) })
	}
}

func channelDown(t *testing.T, cut bool) {
	h := newNewestHLS(t)
	var down atomic.Bool
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if down.Load() && cut {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				c.Close()
			}
			return true
		}
		if down.Load() {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	if n := h.opens.Load(); n != 1 {
		t.Fatalf("the tune opened the channel at the provider %d times, want 1", n)
	}
	down.Store(true)
	asks := 0
	for _, nap := range []time.Duration{0, time.Second, 2 * time.Second} { // a player's load and its two retries
		time.Sleep(nap)
		st, _, _ := devGet(h.base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8", 10*time.Second)
		asks++
		if st == 200 {
			t.Errorf("ask %d while the channel is down got status 200", asks)
		}
		if n := int(h.opens.Load()); n > 1+asks {
			t.Errorf("after ask %d while the channel is down the provider had been asked to open it %d times: more than one open for one ask (the tune and %d asks make %d at most)", asks, n, asks, 1+asks)
		}
	}
	// The player has given up and asks no more: nothing is opened that no player asked for.
	before := h.opens.Load()
	time.Sleep(2500 * time.Millisecond)
	if n := h.opens.Load(); n != before {
		t.Errorf("no player asked for 2.5 s and the channel was opened at the provider %d more time(s)", n-before)
	}
	// The channel is back: the next reload gets its playlist, with no wait.
	down.Store(false)
	start := time.Now()
	st, list, err := devGet(h.base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8", 10*time.Second)
	if err != nil || st != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
		t.Errorf("the reload once the channel was back: status %d, %v, want the playlist", st, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the reload once the channel was back took %.1f s", took.Seconds())
	}
}

// What the one new open is for: a session the provider ended (its address answers 404 from then on) is opened
// again once, and the player gets the new session's playlist on that same reload.
func TestHLSEndedSessionStillReopenedOnce(t *testing.T) {
	h := newNewestHLS(t)
	if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	h.latest.Store(99) // the provider ended the session: its playlist is gone
	h.frontHook = nil
	st, list, err := devGet(h.base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8", 10*time.Second)
	if err != nil || st != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
		t.Errorf("the reload after the session ended: status %d, %v, want the new session's playlist", st, err)
	}
	if n := h.opens.Load(); n != 2 {
		t.Errorf("the channel was opened at the provider %d times, want 2 (the first open and one re-open)", n)
	}
}
