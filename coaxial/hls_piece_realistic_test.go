package main

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// Live HLS pieces as a busy provider sends them: 3 to 8 MB, some taking 2 to 8.7 s to arrive, some slow to
// start. Slow is not quiet: a piece that keeps sending, or answers within a player's own read timeout, is passed
// on from its one ask. (The quiet side is in quiet_connection_test.go and hls_piece_bounded_test.go.)

// A 4 MB piece whose server answers at once and sends it over 8.7 s, longer than pieceWait in all but never
// quiet for long. The player gets the whole piece on its one answer, and the server is asked for it once.
func TestRealisticSlowPieceIsAskedOnce(t *testing.T) {
	const size = 4 << 20 / 188 * 188
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{6.3}, size: size, piece: func(tok, n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			trickle(w, r, tsFill(size/188), 8700*time.Millisecond)
			return true
		}
		return false
	}})
	list := tuneIn(t, p, "192.0.2.1")
	status, data, took := askPiece(p.base, pieceOf(t, list))
	if status != 200 || len(data) != size {
		t.Errorf("a piece sent over 8.7 s: status %d, %d bytes after %.1f s, want the whole piece (%d bytes)", status, len(data), took.Seconds(), size)
	}
	if path, n := p.most(); n != 1 {
		t.Errorf("a piece that kept sending was asked for %d times (%s), want 1", n, path)
	}
}

// A piece whose server is slow to start (6 s before its answer, under a player's own 8 s read timeout) is
// waited for, as a player straight at the server waits for it, and asked for once.
func TestRealisticPieceSlowToStartIsAskedOnce(t *testing.T) {
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{6.3}, piece: func(tok, n int, w http.ResponseWriter, r *http.Request) bool {
		return n == 1 && !waitOr(r, 6*time.Second)
	}})
	list := tuneIn(t, p, "192.0.2.1")
	status, data, took := askPiece(p.base, pieceOf(t, list))
	if status != 200 || len(data) != len(p.data) {
		t.Errorf("a piece slow to start: status %d, %d bytes after %.1f s, want the whole piece", status, len(data), took.Seconds())
	}
	if path, n := p.most(); n != 1 {
		t.Errorf("a piece answered after 6 s was asked for %d times (%s), want 1", n, path)
	}
}

// Pieces of a live channel are asked for every few seconds from one video server. Its name is looked up once a
// minute, not for every piece: a home router's resolver that is slow now and then would otherwise add its delay
// to every piece.
func TestRealisticPieceNameLookedUpOncePerMinute(t *testing.T) {
	fr := newFakeResolver(t, net.ParseIP("198.51.100.7"))
	mu.Lock()
	delete(checks, "steady.example")
	mu.Unlock()
	if !nameOK(t.Context(), "steady.example") {
		t.Fatalf("a name that leads to a public address was refused")
	}
	asked := fr.asked.Load()
	if asked == 0 {
		t.Fatalf("the name was never looked up")
	}
	for i := 0; i < 5; i++ { // the next pieces, a little apart
		time.Sleep(200 * time.Millisecond)
		if !nameOK(t.Context(), "steady.example") {
			t.Fatalf("piece %d: the name was refused", i+2)
		}
	}
	if n := fr.asked.Load() - asked; n != 0 {
		t.Errorf("the resolver was asked %d more times for the next 5 pieces within a second, want 0 (once a minute)", n)
	}
}
