package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cadenceHLS: a provider whose live playlists state target and move every cadence seconds (time-scaled from a
// live evening: one tenth). stuckAfter > 0: the first session stops moving after that many seconds.
func cadenceHLS(t *testing.T, target, cadence, stuckAfter float64) *newestHLS {
	h := newNewestHLS(t)
	began := time.Now()
	var stuckTok atomic.Value // (the session in use when the stall begins: that one stops, a new one moves on)
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		el := time.Since(began).Seconds()
		if stuckAfter > 0 && el > stuckAfter && stuckTok.Load() == nil {
			stuckTok.Store(fmt.Sprintf("/tok%d/", h.latest.Load()))
		}
		seq := 1000 + int(el/cadence)
		if tok, _ := stuckTok.Load().(string); tok != "" && strings.Contains(r.URL.Path, tok) {
			seq = 1000 + int(stuckAfter/cadence)
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:%g\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:%g,\np%d.ts\n", target, seq, cadence, seq)
		return true
	}
	return h
}

// play: device dev tunes and reloads every every seconds for d seconds.
func play(t *testing.T, h *newestHLS, dev string, every, d float64) {
	const own, list = "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
	if st, _, err := devGet(h.base, dev, own, 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	for end := time.Now().Add(time.Duration(d * float64(time.Second))); time.Now().Before(end); {
		devGet(h.base, dev, list, 10*time.Second)
		time.Sleep(time.Duration(every * float64(time.Second)))
	}
}

func scaleCap(t *testing.T) {
	old, oldStill := stallCap, stoodStill
	stallCap, stoodStill = 1.0, 300 // (10 s, at one tenth; the shared limit out of reach)
	t.Cleanup(func() { stallCap, stoodStill = old, oldStill })
}

// Healthy playback as one provider sends it: it states 4 s pieces and its playlist moves every 6.3 s, the player
// reloads every 1.5 s. Nothing is stuck, so the channel is opened at the provider once.
func TestHLSStated4sMoving6sIsNotRescued(t *testing.T) {
	scaleCap(t)
	h := cadenceHLS(t, 0.4, 0.63, 0)
	play(t, h, "192.0.2.1", 0.15, 12)
	if n := h.opens.Load(); n > 3 {
		t.Errorf("healthy playback (4 s stated, a move every 6.3 s) opened the channel %d times at the provider, want at most 3 (the tune and two the channel learns from)", n)
	}
}

// A 24/7 channel stating 11 s pieces whose playlist moves every 11.5 s. 10 s (the cap) is under one piece:
// nothing may be rescued.
func TestHLSStated11sMoving11sIsNotRescued(t *testing.T) {
	scaleCap(t)
	h := cadenceHLS(t, 1.1, 1.15, 0)
	play(t, h, "192.0.2.1", 0.2, 8)
	if n := h.opens.Load(); n > 3 {
		t.Errorf("healthy playback (11 s stated, a move every 11.5 s) opened the channel %d times at the provider, want at most 3 (the tune and two the channel learns from)", n)
	}
}

// The two freezes still get their rescue: 5 s pieces moving every 5 s that stop for good, and 9 s pieces moving
// every 8.5 s that stop for good (the player froze 12.5 s and 11 s in).
func TestHLSRealStallsAreStillRescued(t *testing.T) {
	for _, c := range []struct {
		name                          string
		target, cadence, every, limit float64
	}{
		{"5 s pieces", 0.5, 0.5, 0.2, 0.75}, {"9 s pieces", 0.9, 0.85, 0.2, 1.0},
	} {
		t.Run(c.name, func(t *testing.T) {
			scaleCap(t)
			h := cadenceHLS(t, c.target, c.cadence, 4*c.cadence)
			began := time.Now()
			const own, list = "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
			devGet(h.base, "192.0.2.1", own, 10*time.Second)
			for time.Since(began).Seconds() < 4*c.cadence+2.5 && h.opens.Load() < 2 {
				devGet(h.base, "192.0.2.1", list, 10*time.Second)
				time.Sleep(time.Duration(c.every * float64(time.Second)))
			}
			if h.opens.Load() < 2 {
				t.Fatalf("a stalled session (%s) was never rescued", c.name)
			}
			at := time.Since(began).Seconds() - 4*c.cadence
			t.Logf("%s: rescued %.1f s (x10) after the playlist stopped", c.name, at*10)
			// At the first reload past the limit after the last move seen (moves are seen at reloads too): one
			// reload late at most for each. The freezes came 11 and 12.5 s in.
			if at > c.limit+2*c.every+0.15 {
				t.Errorf("a stalled session (%s) was rescued %.1f s (x10) after it stopped, later than the first reload past its limit", c.name, at*10)
			}
		})
	}
}

// The real pace of a channel on a live evening, replayed (one tenth of the time): the seconds between moves of
// its playlist, as Coaxial logged them, with a lone player reloading. Normal playback; after the channel has
// learned its pace (at most two wasted rescues) nothing more is opened.
func TestHLSRealPaceReplayedIsNotRescued(t *testing.T) {
	for _, c := range []struct {
		name   string
		target float64
		gaps   []float64
	}{
		{"stated 4 s", 0.4, []float64{0.66, 0.64, 0.42, 0.64, 0.64, 0.65, 0.63, 0.65, 0.44, 0.65, 0.65, 0.63, 0.65, 0.43, 0.65, 0.64, 0.65, 0.63, 0.65, 0.43, 0.65, 0.63, 0.65, 0.63, 0.65, 0.42, 0.64, 0.64, 0.65, 0.64, 0.44, 0.63, 0.65, 0.64, 0.65, 0.64, 0.44, 0.80, 0.64, 0.66}},
		{"stated 11 s", 1.1, []float64{1.26, 0.72, 1.26, 1.27, 0.77, 1.30, 0.75, 1.31, 0.81, 1.33, 1.11, 1.10, 1.10, 1.10, 1.10, 1.10, 1.10, 1.10, 1.10}},
	} {
		t.Run(c.name, func(t *testing.T) {
			scaleCap(t)
			h := newNewestHLS(t)
			began := time.Now()
			at := []float64{0}
			for _, g := range c.gaps {
				at = append(at, at[len(at)-1]+g)
			}
			h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
				el, seq := time.Since(began).Seconds(), 0
				for seq < len(at)-1 && at[seq+1] <= el {
					seq++
				}
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:%g\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:%g,\np%d.ts\n", c.target, 1000+seq, c.target, seq)
				return true
			}
			play(t, h, "192.0.2.1", 0.2, at[len(at)-1])
			if n := h.opens.Load(); n > 3 {
				t.Errorf("the real pace of a channel (%s) opened it %d times at the provider, want at most 3", c.name, n)
			}
		})
	}
}
