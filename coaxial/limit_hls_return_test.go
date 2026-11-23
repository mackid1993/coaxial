package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A live HLS session nobody asked for in hlsHeld no longer counts, and its connection was given to another
// device. The device coming back to it is a new connection: the rules apply, as for any other.
func TestLimitHLSSessionComingBack(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" {
			http.Redirect(w, r, "/hlsr/tok44/realU/realP/44/index.m3u8", 302)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n44_1.ts\n")
			return true
		}
		return false
	})
	f, base := advFront(t, fp, "hls", advanced{Connections: "1"})
	r := ask(t, base, "192.0.2.1", "/live/tv/easy/44.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("live HLS: %d %q", r.StatusCode, text)
	}
	mu.Lock() // the player stops asking (paused, or the TV went to sleep) for longer than hlsHeld
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= hlsHeld + 1
			for d := range s.who {
				s.who[d] -= hlsHeld + 1
			}
		}
	}
	mu.Unlock()
	film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
	if film.StatusCode != 200 {
		t.Fatalf("another device once the session no longer counts: %d %q", film.StatusCode, body(film))
	}
	defer film.Body.Close()
	before := fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8")
	back := ask(t, base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8")
	if text := body(back); back.StatusCode != 503 {
		t.Errorf("the first device coming back while the only connection is in use: %d %q", back.StatusCode, text)
	}
	if fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8") != before {
		t.Errorf("the provider was asked for the session over the limit")
	}
}
