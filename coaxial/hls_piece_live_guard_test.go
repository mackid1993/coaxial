package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A live piece sent without a Content-Length (in chunks) that comes whole and ends as it should. It can be
// carried on if it stops part way (resumeBody), and that must cost a healthy piece nothing: the player gets
// exactly the piece, at once, and its server is asked once.
func TestHLSLivePieceWithoutLengthEndsNormally(t *testing.T) {
	pkt := append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...)
	piece := bytes.Repeat(pkt, pieceSize/188)
	var asks atomic.Int32
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") {
			return false
		}
		asks.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		for off := 0; off < len(piece); off += len(piece) / 4 { // (no length: sent in chunks)
			w.Write(piece[off:min(off+len(piece)/4, len(piece))])
			w.(http.Flusher).Flush()
		}
		return true
	})
	_, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	status, data, took := askPiece(base, pieceOf(t, list))
	if status != 200 || !bytes.Equal(data, piece) {
		t.Errorf("piece: status %d, %d bytes, want 200 and the %d bytes its server sent", status, len(data), len(piece))
	}
	if took > 3*time.Second {
		t.Errorf("a piece that came whole took %.1f s to pass on", took.Seconds())
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("the piece's server was asked %d times for a piece that came whole, want 1", n)
	}
}

// The list of what live playlists named (livePieces) is written whenever a live playlist is handed out and read
// for every other request. Both happen at once all evening: a device on a live HLS channel while another opens a
// film, a catch-up or the guide. Read without the lock, that is a fault Go ends the whole program for ("concurrent
// map read and map write"), which would drop every viewer and recording. This test does both at once; run with
// the race detector it reports any read or write made without the lock.
func TestLivePieceListSharedSafely(t *testing.T) {
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/hlsr/tok44/index.m3u8" {
			return false
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\np1.ts\n#EXTINF:4,\np2.ts\n")
		return true
	})
	prov := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/live/realU/realP/44.m3u8":
			http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok44/index.m3u8", 302)
		case "/movie/realU/realP/7.mp4": // (a very short film, answered at once, so that many can be asked for)
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(make([]byte, 188))
		default:
			return false
		}
		return true
	})
	_, base := limFront(t, prov, "hls", "", advanced{})
	if s, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8"); s != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
		t.Fatalf("the live playlist: %d %q", s, list)
	}
	stop := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	var films atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() { // a player reloading its live playlist
			defer wg.Done()
			for time.Now().Before(stop) {
				playerGet(t, base, "/live/tv/easy/44.m3u8")
			}
		}()
		go func(dev string) { // another device asking for a film, which is no piece of any playlist
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			for time.Now().Before(stop) {
				req, _ := http.NewRequest("GET", base+"/movie/tv/easy/7.mp4", nil)
				req.Header.Set("X-Dev", dev)
				if resp, err := client.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					films.Add(1)
				}
			}
		}("192.0.2." + string(rune('2'+i)))
	}
	wg.Wait()
	if n := films.Load(); n < 200 {
		t.Fatalf("only %d film requests were answered: the test did not do the two things at once", n)
	}
	t.Logf("%d film requests were answered while live playlists were handed out", films.Load())
}
