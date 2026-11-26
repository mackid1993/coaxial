package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A recorder reads MPEG-TS that Coaxial makes from a channel's HLS (the bridge). For a few seconds the video
// server answers 404 for every piece (a channel restarting on the provider's side, as on MPEG-TS, where a
// watched channel is asked for again for about a minute), while the playlist goes on listing new ones. Players
// and tools that read HLS skip a piece they cannot get and carry on with the playlist (FFmpeg's hls demuxer:
// "failed too many times, skipping"; Media3 retries the piece, then goes on). The bridge must do the same: the
// recording goes on once pieces come again. It ended the channel after three pieces missed in a row, which a
// video server answering 404 at once makes happen within a second.
func TestBridgeCarriesOnAfterMissedPieces(t *testing.T) {
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 500)
	start := time.Now()
	var bad atomic.Bool
	var served, refused atomic.Int32
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/hlsr/tok1/") {
			return false
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") { // a live playlist: 1 s pieces, the newest three, one more each second
			n := int(time.Since(start).Seconds())
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", 100+n)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "#EXTINF:1,\np%d.ts\n", 100+n+i)
			}
			return true
		}
		if bad.Load() {
			refused.Add(1)
			http.NotFound(w, r)
			return true
		}
		served.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(piece)
		return true
	})
	prov := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/44.m3u8" {
			return false
		}
		http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok1/index.m3u8", 302)
		return true
	})
	_, base := limFront(t, prov, "hls", "", advanced{})
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/44.ts")
	defer rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Fatalf("recorder: %d", rec.StatusCode)
	}
	d := drain(rec)
	d.flowing(t, "the recording before the pieces go missing", 3*time.Second)
	bad.Store(true)
	time.Sleep(5 * time.Second) // five pieces in a row answer 404
	bad.Store(false)
	if refused.Load() < 3 {
		t.Fatalf("the fake refused only %d piece asks: the test did not act out three missed pieces", refused.Load())
	}
	was := served.Load()
	time.Sleep(4 * time.Second)
	if served.Load() == was {
		t.Errorf("no piece was asked for after the video server came back: the bridge had given the channel up")
	}
	d.flowing(t, "the recording after the video server came back", 3*time.Second)
}

// A provider connection that sent nothing for too long is said so in the log, in the words AGENTS.md rule 14
// tells readers to look for, not as "OSError".
func TestQuietConnectionNamedInLog(t *testing.T) {
	if got, want := endReason(timeoutError{}), "the provider sent nothing for too long"; got != want {
		t.Errorf("a quiet connection is logged as %q, want %q", got, want)
	}
}
