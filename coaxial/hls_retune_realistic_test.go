package main

import (
	"net/http"
	"testing"
	"time"
)

// Some players ask for the channel's own address at every playlist refresh (they follow the redirect each time
// instead of keeping the address it leads to; see "once a minute" in playlistRequest). On a provider that states
// 4 s pieces while its playlist moves every 6.3 s, playback is healthy and nothing is stuck. The rescue that
// fires on a reload learns the channel's pace after at most two wasted reopens
// (TestHLSStated4sMoving6sIsNotRescued); the same player on the re-tune door must not reopen the channel at the
// provider at every move either: each reopen ends the session (newest wins), its pieces answer 404, and some
// providers refuse reopening for minutes. (One tenth of the time.)
func TestRealisticPlayerAskingTheChannelAtEveryRefreshIsNotRescued(t *testing.T) {
	scaled(t, 1.0, 300)
	p := newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.63}})
	for time.Since(p.began).Seconds() < 12 {
		tuneIn(t, p, "192.0.2.1") // the channel's own address, then the playlist it leads to
		time.Sleep(150 * time.Millisecond)
	}
	if n := p.opens.Load(); n > 3 {
		t.Errorf("healthy playback (4 s stated, a move every 6.3 s) by a player that asks for the channel at every refresh opened it %d times at the provider, want at most 3", n)
	}
}

// The same player on a session that really stopped at the provider, which refuses the reopen (403, as one does
// for minutes after a reopen). The reload door asks the provider once and then passes the old answer on for
// renewGap (TestRealisticRefusedRescueKeepsThePlaylist); the re-tune door must not ask the provider to open the
// channel at every refresh while the refusal stands either. (One tenth of the time; renewGap is 8 s.)
func TestRealisticRefusedRescueNotAskedAtEveryRefresh(t *testing.T) {
	scaled(t, 1.0, 300)
	p := newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			http.Error(w, "Forbidden", 403)
			return true
		}
		return false
	}})
	tuneIn(t, p, "192.0.2.1")
	for end := time.Now().Add(800 * time.Millisecond); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		devGet(p.base, "192.0.2.1", listAddr, 10*time.Second)
	}
	p.stick(1)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		tuneIn(t, p, "192.0.2.1")
	}
	if n := p.opens.Load(); n > 2 {
		t.Errorf("the channel was asked for at the provider %d times in 3 s of a refused rescue by a player asking for it at every refresh; the tune and one rescue make 2 (renewGap is 8 s)", n)
	}
}
