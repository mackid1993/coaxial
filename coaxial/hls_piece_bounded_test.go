package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// No wait on a live HLS piece may be longer than a player's own (AGENTS.md rule 14). After the first quiet
// connection a piece is asked again at once on a new one; that new connection used to get the full 60 s, so a
// server that stayed quiet held the player 68 s, and a piece resumed part way could wait 60 s for its Range.

// The piece's server says nothing on the first connection and on the new one: the player is answered within
// two pieceWaits, not after a minute.
func TestHLSPieceQuietTwiceAnsweredWithinTwoPieceWaits(t *testing.T) {
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(path, ".ts") && n <= 2 {
			<-r.Context().Done()
			return true
		}
		return false
	})
	status, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	if status != 200 {
		t.Fatalf("playlist: %d", status)
	}
	status, _, took := playerGet(t, base, pieceOf(t, list))
	if took > secs(2*pieceWait+4) {
		t.Errorf("a piece whose server stayed quiet held the player %.1f s; want at most %d s", took.Seconds(), 2*pieceWait+4)
	}
	if status != 504 {
		t.Errorf("a piece whose server stayed quiet: status %d, want 504", status)
	}
}

// The piece stops part way and the Range asked for the rest is not answered either: the player's answer ends
// (broken, so it knows) within two pieceWaits.
func TestHLSPieceResumeQuietEndsWithinTwoPieceWaits(t *testing.T) {
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") {
			return false
		}
		if n == 1 {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Length", fmt.Sprint(1<<20/188*188))
			w.Write(bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1500))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done() // (and the Range for the rest gets no answer at all)
		return true
	})
	_, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	status, data, took := playerGet(t, base, pieceOf(t, list))
	if took > secs(2*pieceWait+4) {
		t.Errorf("a piece stopped part way, its rest never answered, held the player %.1f s; want at most %d s", took.Seconds(), 2*pieceWait+4)
	}
	if status == 200 && len(data) == 1<<20/188*188 {
		t.Errorf("the player was handed a whole piece that was never sent")
	}
}

// A piece asked for after its session was let go or replaced (a renewal: the channel opened again at the
// provider) is still a live piece: the playlist named it a moment ago, so it gets the same short waits.
func TestHLSPieceOfARenewedSessionKeepsShortWaits(t *testing.T) {
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(path, ".ts") && n == 1 {
			<-r.Context().Done()
			return true
		}
		return false
	})
	status, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	if status != 200 {
		t.Fatalf("playlist: %d", status)
	}
	mu.Lock() // the session is replaced, as a renewal does
	for k := range sessions {
		delete(sessions, k)
	}
	mu.Unlock()
	status, data, took := playerGet(t, base, pieceOf(t, list))
	if status != 200 || len(data) != 1<<20/188*188 {
		t.Errorf("the piece after its session was replaced: status %d, %d bytes, want the whole piece", status, len(data))
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece after its session was replaced took %.1f s; a quiet server must be asked again after %d s", took.Seconds(), pieceWait)
	}
}

// The first look-up of a video server's name (a server not seen before) waits on the resolver no longer than a
// later one does: with a resolver that says nothing it held the piece up to connectWait (10 s). The name is then
// left to the check made when the connection is dialed, and looked up again soon.
func TestHLSPieceFirstLookupIsBounded(t *testing.T) {
	fr := newFakeResolver(t, net.ParseIP("198.51.100.7"))
	mu.Lock()
	delete(checks, "first.example")
	mu.Unlock()
	fr.mode.Store(2)
	began := time.Now()
	ok := nameOK(t.Context(), "first.example")
	took := time.Since(began)
	fr.mode.Store(0)
	if took > 4*time.Second {
		t.Errorf("the first look-up of a name held the piece %.1f s with a resolver that said nothing", took.Seconds())
	}
	if !ok {
		t.Errorf("a name with no answer yet was refused; the connect-time check covers it")
	}
	aMinuteLater()
	if !nameOK(t.Context(), "first.example") {
		t.Errorf("the name was refused once the resolver answered")
	}
}
