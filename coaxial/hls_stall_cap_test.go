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

// A 24/7 channel with 9 s pieces: 1.5 target durations is 13.5 s, but the player froze after 11 s of a stalled
// playlist. A device alone on the session gets a new one once the playlist has stood still stallCap seconds,
// however long the pieces are. (stallCap is shortened here; the pieces stay 9 s long.)
func TestHLSStalledLongPiecePlaylistIsRenewedAtTheCap(t *testing.T) {
	old := stallCap
	stallCap = 1.0
	t.Cleanup(func() { stallCap = old })
	h := &standstillHLS{newestHLS: newNewestHLS(t)}
	oldStill := stoodStill
	stoodStill = 300
	t.Cleanup(func() { stoodStill = oldStill })
	var stuck atomic.Bool
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if stuck.Load() && strings.Contains(r.URL.Path, "/tok1/") {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:9\n#EXT-X-MEDIA-SEQUENCE:500\n#EXTINF:8.5,\np500.ts\n")
			return true
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:9\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:8.5,\np%d.ts\n", 100+n, 100+n)
		return true
	}
	const dev, own, list = "192.0.2.1", "/live/tv/easy/44.m3u8", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"
	if st, _, err := devGet(h.base, dev, own, 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	devGet(h.base, dev, list, 10*time.Second)
	stuck.Store(true)
	moved := false
	for i := 0; i < 8 && !moved; i++ {
		_, data, _ := devGet(h.base, dev, list, 10*time.Second)
		moved = !bytes.Contains(data, []byte("MEDIA-SEQUENCE:500"))
		time.Sleep(400 * time.Millisecond)
	}
	if !moved || h.opens.Load() != 2 {
		t.Errorf("a stalled playlist with 9 s pieces was not renewed at the cap (%d open(s) at the provider, want 2)", h.opens.Load())
	}
}
