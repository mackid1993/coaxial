package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// On a provider whose players use HLS, a recorder asking for MPEG-TS of a channel another device watches as
// HLS reads the same provider session (the relay builds its MPEG-TS from that channel's HLS): it costs no
// connection, and is not refused at the limit.
func TestLimitTSJoinsHLSSession(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" {
			http.Redirect(w, r, "/hlsr/tok44/realU/realP/44/index.m3u8", 302)
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/hlsr/") && strings.HasSuffix(r.URL.Path, ".ts") {
			fp.stream(w, r, "piece", true, 188*1000)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n44_1.ts\n")
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "hls", advanced{Connections: "1"})
	r := ask(t, base, "192.0.2.1", "/live/tv/easy/44.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("live HLS: %d %q", r.StatusCode, text)
	}
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/44.ts")
	defer rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Errorf("a recorder on the channel the TV watches, at the limit: %d %q", rec.StatusCode, body(rec))
	}
	if n := fp.count(fp.hits, "/live/realU/realP/44.m3u8"); n != 1 {
		t.Errorf("the provider was asked for the channel %d times, want 1 (one session)", n)
	}
}
