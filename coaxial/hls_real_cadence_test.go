package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cadenceHLS: a provider whose live playlists state target and move every cadence seconds (time-scaled from a
// live evening: one tenth). stuckAfter > 0: the first session stops moving after that many seconds.
func cadenceHLS(t *testing.T, target, cadence, stuckAfter float64) *newestHLS {
	h, _ := cadenceHLSSeen(t, target, cadence, stuckAfter)
	return h
}

// cadenceHLSSeen: cadenceHLS, and the time a player was first handed the last playlist of the session that
// stalls (the last move the player saw).
func cadenceHLSSeen(t *testing.T, target, cadence, stuckAfter float64) (*newestHLS, *atomic.Int64) {
	h := newNewestHLS(t)
	began := time.Now()
	var seen atomic.Int64
	served := map[string]int{}
	var mu sync.Mutex
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
		tok := tokRe.FindString(r.URL.Path)
		mu.Lock()
		if seq > served[tok] {
			served[tok] = seq
			if st, _ := stuckTok.Load().(string); st == "" || st == tok {
				seen.Store(time.Now().UnixNano())
			}
		}
		mu.Unlock()
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:%g\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:%g,\np%d.ts\n", target, seq, cadence, seq)
		return true
	}
	return h, &seen
}

var tokRe = regexp.MustCompile(`/tok[0-9]+/`)

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
		{"5 s pieces", 0.5, 0.5, 0.2, 0.94}, {"9 s pieces", 0.9, 0.85, 0.2, 1.0},
	} {
		t.Run(c.name, func(t *testing.T) {
			scaleCap(t)
			h, seen := cadenceHLSSeen(t, c.target, c.cadence, 4*c.cadence)
			began := time.Now()
			before := int32(-1) // (opens when the stall began: a wasted rescue before it does not count)
			const own, list = "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
			devGet(h.base, "192.0.2.1", own, 10*time.Second)
			for time.Since(began).Seconds() < 4*c.cadence+2.5 && (before < 0 || h.opens.Load() <= before) {
				if before < 0 && time.Since(began).Seconds() > 4*c.cadence {
					before = h.opens.Load()
				}
				devGet(h.base, "192.0.2.1", list, 10*time.Second)
				time.Sleep(time.Duration(c.every * float64(time.Second)))
			}
			if before < 0 || h.opens.Load() <= before {
				t.Fatalf("a stalled session (%s) was never rescued", c.name)
			}
			// From the last move the player was handed, as it counts a standstill: the rescue comes on its first
			// reload past the limit (10 s for 9 s pieces). The freezes came 11 and 12.5 s in.
			at := time.Since(time.Unix(0, seen.Load())).Seconds()
			t.Logf("%s: rescued %.1f s (x10) after the last move the player saw", c.name, at*10)
			if at > c.limit+c.every+0.05 {
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

// The other way in: a player that asks for the channel's own address again every few seconds (some do at each
// refresh) while its playlist moves at a provider's healthy pace. A rescue through that path that finds the
// provider no further on must teach the channel its pace too, or it reopens the channel for as long as it plays.
func TestHLSRetunesAtRealPaceAreNotRescued(t *testing.T) {
	for _, c := range []struct {
		name            string
		target, cadence float64
	}{{"stated 11 s, moving every 12 s", 1.1, 1.2}, {"stated 4 s, moving every 6.6 s", 0.4, 0.66}} {
		t.Run(c.name, func(t *testing.T) {
			scaleCap(t)
			h := cadenceHLS(t, c.target, c.cadence, 0)
			const own, list = "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
			devGet(h.base, "192.0.2.1", own, 10*time.Second)
			for i, end := 0, time.Now().Add(24*time.Second); time.Now().Before(end); i++ {
				p := list
				if i%3 == 2 { // (about every 0.5 s: its own address again)
					p = own
				}
				devGet(h.base, "192.0.2.1", p, 10*time.Second)
				time.Sleep(200 * time.Millisecond)
			}
			if n := h.opens.Load(); n > 3 {
				t.Errorf("a player asking its channel's own address again at a healthy pace (%s) had it opened %d times at the provider in 4 minutes (x10), want at most 3", c.name, n)
			}
		})
	}
}

// A channel's pace, once learned, is kept for its next tune: tuning it again later (the session long gone) does
// not cost a new session at the provider to learn it again.
func TestHLSLearnedPaceIsKeptForTheNextTune(t *testing.T) {
	scaleCap(t)
	h := cadenceHLS(t, 0.4, 0.66, 0)
	play(t, h, "192.0.2.1", 0.15, 8) // (it learns its pace)
	mu.Lock()
	for k := range sessions { // (the session forgotten, as a minute after the player left)
		delete(sessions, k)
	}
	mu.Unlock()
	before := h.opens.Load()
	play(t, h, "192.0.2.1", 0.15, 8)
	if n := h.opens.Load() - before; n != 1 {
		t.Errorf("tuning a channel whose pace was learned opened it %d times at the provider, want 1 (the tune)", n)
	}
}

// The same for a player that also asks its channel's own address again at its refreshes: the pace its rescue
// learned is kept for the channel's next tune too.
func TestHLSLearnedPaceThroughRetunesIsKeptForTheNextTune(t *testing.T) {
	scaleCap(t)
	h := cadenceHLS(t, 0.4, 0.66, 0)
	const own, list = "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
	retune := func(d time.Duration) {
		devGet(h.base, "192.0.2.1", own, 10*time.Second)
		for i, end := 0, time.Now().Add(d); time.Now().Before(end); i++ {
			p := list
			if i%3 == 2 {
				p = own
			}
			devGet(h.base, "192.0.2.1", p, 10*time.Second)
			time.Sleep(200 * time.Millisecond)
		}
	}
	retune(10 * time.Second) // (it learns its pace)
	mu.Lock()
	for k := range sessions {
		delete(sessions, k)
	}
	mu.Unlock()
	before := h.opens.Load()
	retune(8 * time.Second)
	if n := h.opens.Load() - before; n != 1 {
		t.Errorf("tuning again a channel whose pace was learned through re-tunes opened it %d times at the provider, want 1 (the tune)", n)
	}
}

// A channel stating 5 s pieces whose playlist moves every 6.3 s and now and then stands still 10.3 to 11.1 s in
// healthy playback (the real pace of one, one tenth of the time). Held at stallCap (10 s) it was opened again at
// each long pause; it learns past that, up to 12 s, so after one wasted reopen it is left alone.
func TestHLSShortPieceChannelLearnsPastTheCap(t *testing.T) {
	scaleCap(t)
	h := newNewestHLS(t)
	gaps := []float64{0.64, 0.63, 0.65, 0.64, 1.03, 0.63, 0.65, 0.64, 0.66, 1.05, 0.63, 0.64, 0.65, 0.63, 1.11,
		0.64, 0.63, 0.65, 0.64, 1.06, 0.63, 0.65, 0.64, 0.63, 1.08, 0.64, 0.65, 0.63}
	at := []float64{0}
	for _, g := range gaps {
		at = append(at, at[len(at)-1]+g)
	}
	began := time.Now()
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		el, seq := time.Since(began).Seconds(), 0
		for seq < len(at)-1 && at[seq+1] <= el {
			seq++
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:0.5\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:0.5,\np%d.ts\n", 1000+seq, seq)
		return true
	}
	play(t, h, "192.0.2.1", 0.2, at[len(at)-1])
	if n := h.opens.Load(); n > 2 {
		t.Errorf("a 5 s channel whose healthy pauses reach 11 s was opened %d times at the provider, want at most 2 (the tune and one it learns from)", n)
	}
}
