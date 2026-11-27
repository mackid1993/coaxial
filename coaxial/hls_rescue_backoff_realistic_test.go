package main

import (
	"bytes"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Every way a rescue (the channel opened again in place of a session that stopped, or that failed) can come to
// nothing must hold the next one off for renewGap, on both doors: some providers refuse reopening a channel for
// minutes, so a reopen at every reload only keeps the refusal going. And the player is handed the stalled
// session's own playlist each time, never the provider's refusal. The stand-in is newRealProv: newest session
// wins, and each case is one way a real provider was seen to answer. (One tenth of the time: 4 s pieces,
// stallCap 10 s, a reload every 1.5 s; renewGap stays 8 s.)

// rbHangUp: the provider's own address takes the connection and closes it without an answer (a dropped
// connection on the way through the tunnel).
func rbHangUp(w http.ResponseWriter) {
	if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
		c.Close()
	}
}

// rbStall: the device tunes in and plays a moment, then the session stops at the provider (it still answers
// 200, the same playlist every time). Returns the opens at the provider when the stall began.
func rbStall(t *testing.T, p *realProv, dev string) int32 {
	t.Helper()
	tuneIn(t, p, dev)
	for end := time.Now().Add(800 * time.Millisecond); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		devGet(p.base, dev, listAddr, 10*time.Second)
	}
	p.stick(int(p.latest.Load()))
	return p.opens.Load()
}

// rbReloads: the device reloads its playlist every 150 ms for d. Every answer must be a playlist with pieces:
// what the player had, or a new session's.
func rbReloads(t *testing.T, p *realProv, dev string, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		st, data, err := devGet(p.base, dev, listAddr, 10*time.Second)
		if err != nil || st != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
			t.Fatalf("a reload while the rescue came to nothing: status %d, %v, %q; want the playlist the player had", st, err, data)
		}
	}
}

// The reopen gets no answer at all (the connection is dropped): the stalled session's playlist goes on, and the
// channel is not asked for again at every reload. Before, a reviewer counted 17 opens in 3 s.
func TestRealisticRescueWithNoAnswerIsNotTriedAtEveryReload(t *testing.T) {
	scaled(t, 1.0, 300)
	p := newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			rbHangUp(w)
			return true
		}
		return false
	}})
	from := rbStall(t, p, "192.0.2.1")
	rbReloads(t, p, "192.0.2.1", 3*time.Second)
	if n := p.opens.Load() - from; n < 1 {
		t.Fatalf("the stalled session was never rescued")
	} else if n > 1 {
		t.Errorf("a rescue the provider dropped was tried %d times in 3 s of reloads; want 1 (renewGap is 8 s)", n)
	}
}

// The reopen is sent on to a new session whose own playlist answers 403 (some providers refuse at the video
// server, not at the channel's address). The reload that made the rescue is handed the stalled session's
// playlist, not the 403, and the channel is not opened again at every reload after it. Later reloads may get
// the provider's 403: where the newest session wins, the reopen has already ended the stalled one, so there is
// nothing valid left to hand back, as for a player straight at the provider. What this guards is that the
// rescue is not tried again at every reload.
func TestRealisticRescueWhoseNewSessionRefusesKeepsThePlaylist(t *testing.T) {
	scaled(t, 1.0, 300)
	p := newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, playlist: func(tok, n int, w http.ResponseWriter, r *http.Request) bool {
		if tok > 1 {
			http.Error(w, "Forbidden", 403)
			return true
		}
		return false
	}})
	const dev = "192.0.2.1"
	from := rbStall(t, p, dev)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		before := p.opens.Load()
		st, data, err := devGet(p.base, dev, listAddr, 10*time.Second)
		if p.opens.Load() > before && (err != nil || st != 200 || !bytes.Contains(data, []byte("#EXTINF"))) {
			t.Errorf("the reload whose rescue was refused got status %d, %v, %q; want the playlist the player had", st, err, data)
		}
	}
	if n := p.opens.Load() - from; n < 1 {
		t.Fatalf("the stalled session was never rescued")
	} else if n > 1 {
		t.Errorf("a rescue whose new session answered 403 was tried %d times in 3 s of reloads; want 1 (renewGap is 8 s)", n)
	}
}

// The reopen is answered with a playlist at the channel's own address instead of a new session to move to (a
// provider that sends some opens on and answers others itself). That rescued nothing: the stalled session goes
// on, and the channel is not asked for again at every reload.
func TestRealisticRescueAnsweredWithoutASessionIsNotTriedAtEveryReload(t *testing.T) {
	scaled(t, 1.0, 300)
	var p *realProv
	p = newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:0.4\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:0.4,\nq.ts\n", p.seqAt(time.Since(p.began).Seconds()))
			return true
		}
		return false
	}})
	from := rbStall(t, p, "192.0.2.1")
	rbReloads(t, p, "192.0.2.1", 3*time.Second)
	if n := p.opens.Load() - from; n < 1 {
		t.Fatalf("the stalled session was never rescued")
	} else if n > 1 {
		t.Errorf("a rescue answered without a new session was tried %d times in 3 s of reloads; want 1 (renewGap is 8 s)", n)
	}
}

// The same answer on the re-tune door: a player that asks for the channel's own address at every refresh.
func TestRealisticRetuneAnsweredWithoutASessionIsNotTriedAtEveryRefresh(t *testing.T) {
	scaled(t, 1.0, 300)
	var p *realProv
	p = newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:0.4\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:0.4,\nq.ts\n", p.seqAt(time.Since(p.began).Seconds()))
			return true
		}
		return false
	}})
	from := rbStall(t, p, "192.0.2.1")
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		tuneIn(t, p, "192.0.2.1")
	}
	if n := p.opens.Load() - from; n > 1 {
		t.Errorf("a re-tune rescue answered without a new session was tried %d times in 3 s of refreshes; want at most 1 (renewGap is 8 s)", n)
	}
}

// A session the provider ended (its playlist answers 404: a newer one won) is opened again once, as designed;
// when that reopen gets no answer (the provider's address drops the connection while it restarts the channel),
// the next reloads do not each open the channel again: once per renewGap, as a player retrying its own playlist
// at most every 8 s would (hls.js maxRetryDelayMs).
func TestRealisticReopenWithNoAnswerIsNotTriedAtEveryReload(t *testing.T) {
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{6.3}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			rbHangUp(w)
			return true
		}
		return false
	}})
	tuneIn(t, p, "192.0.2.1")
	p.latest.Store(99) // (the session is gone at the provider)
	asks := 0
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		devGet(p.base, "192.0.2.1", listAddr, 10*time.Second)
		asks++
	}
	if n := p.opens.Load(); n > 2 {
		t.Errorf("%d reloads of a session that is gone, whose reopen got no answer, opened the channel %d times; want 2 (the tune and one reopen in renewGap)", asks, n)
	}
}

// The other direction: the hold-off is a pause, not giving up. A stalled session whose first rescue got no
// answer is rescued again once renewGap has passed (real seconds), the player is then handed the moving session,
// and nothing is opened beyond that.
func TestRealisticRescueIsTriedAgainAfterRenewGap(t *testing.T) {
	scaled(t, 1.0, 300)
	var dropped atomic.Bool
	p := newRealProv(t, realCfg{target: 0.4, gaps: []float64{0.4}, front: func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 2 {
			dropped.Store(true)
			rbHangUp(w)
			return true
		}
		return false
	}})
	const dev = "192.0.2.1"
	from := rbStall(t, p, dev)
	stuckAt := p.seqAt(time.Since(p.began).Seconds())
	moved := false
	for end := time.Now().Add(renewGap*time.Second + 2*time.Second); time.Now().Before(end) && !moved; time.Sleep(300 * time.Millisecond) {
		st, data, err := devGet(p.base, dev, listAddr, 10*time.Second)
		if err != nil || st != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
			t.Fatalf("a reload: status %d, %v, %q", st, err, data)
		}
		moved = !bytes.Contains(data, []byte(fmt.Sprintf("MEDIA-SEQUENCE:%d\n", stuckAt)))
	}
	if !dropped.Load() {
		t.Fatalf("the first rescue never came")
	}
	if !moved {
		t.Errorf("after a rescue that got no answer the stalled session was never rescued again (%d opens since the stall)", p.opens.Load()-from)
	}
	if n := p.opens.Load() - from; n != 2 {
		t.Errorf("the stall was rescued with %d opens; want 2 (the one that got no answer and one after renewGap)", n)
	}
}
