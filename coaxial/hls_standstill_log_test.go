package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// How long a live playlist stood still was read from the timing of the players' requests, after the fact, and
// that reading was wrong by 9 s once. Coaxial reads every playlist it hands out, so it writes the standstill
// itself: longer than 1.5 target durations at the detailed level, longer than the three target durations a
// player keeps in hand at the normal level, and not at all when nobody asked for the playlist in between.
func TestLivePlaylistStandstillIsLogged(t *testing.T) {
	b := captureLog(t)
	s := &session{what: what{"ch", "44"}}
	pl := func(seq int) []byte {
		return []byte(fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:1,\np.ts\n", seq))
	}
	read := func(seq int) { mu.Lock(); s.note(pl(seq)); mu.Unlock() }
	still := func(d time.Duration, seq int) { // asked every 0.4 s while it stands still
		for end := time.Now().Add(d); time.Now().Before(end); {
			read(seq)
			time.Sleep(400 * time.Millisecond)
		}
	}
	read(1)
	read(2) // (moved on at once: no line above the everything level)
	still(2200*time.Millisecond, 2)
	read(3) // stood ~2.2 s: over 1.5 target durations, under 3
	still(3400*time.Millisecond, 3)
	read(4) // stood ~3.4 s: over 3 target durations
	time.Sleep(2500 * time.Millisecond)
	read(5) // nobody asked for 2.5 s: not a standstill anyone saw
	log := b.String()
	if got := linesWith(log, " DBG ", "stood still", "before it moved on", "channel=44"); len(got) != 1 {
		t.Errorf("want one detailed line for the 2.2 s standstill, got %d:\n%s", len(got), log)
	}
	if got := linesWith(log, " INF ", "longer than the 3 s a player keeps in hand", "channel=44"); len(got) != 1 {
		t.Errorf("want one normal-level line for the 3.4 s standstill, got %d:\n%s", len(got), log)
	}
	if n := strings.Count(log, "stood still"); n != 2 {
		t.Errorf("want exactly two standstill lines (none for a move nobody waited on), got %d:\n%s", n, log)
	}
}
