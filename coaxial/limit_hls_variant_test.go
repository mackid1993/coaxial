package main

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// A live HLS channel whose playlist is a list of variants (a master playlist): the player then asks only for
// the variant's own playlist, at the address the master playlist named, and the session goes on at the
// provider. It counts as long as the variant is asked for, wherever the variant is, and that request is no new
// connection. (Two servers: the variant on another server, and on the provider's own.)
func TestLimitHLSVariantKeepsSession(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n45_1.ts\n"
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\ntracks-v1/mono.m3u8\n"
	variant := regexp.MustCompile(`(?m)^(/\S+mono\.m3u8)$`)
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/index.m3u8") {
			io.WriteString(w, master)
		} else {
			io.WriteString(w, media)
		}
		return true
	})
	for _, elsewhere := range []bool{true, false} {
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == "/live/realU/realP/45.m3u8" && elsewhere:
				http.Redirect(w, r, cdn.srv.URL+"/edge/45/index.m3u8?t=1", 302)
			case r.URL.Path == "/live/realU/realP/45.m3u8":
				http.Redirect(w, r, "/hlsr/tok45/realU/realP/45/index.m3u8", 302)
			case strings.HasSuffix(r.URL.Path, "/index.m3u8"):
				io.WriteString(w, master)
			case strings.HasSuffix(r.URL.Path, ".m3u8"):
				io.WriteString(w, media)
			default:
				return false
			}
			return true
		})
		_, base := advFront(t, fp, "hls", advanced{Connections: "1"})
		r := ask(t, base, "192.0.2.1", "/live/tv/easy/45.m3u8")
		for r.StatusCode == 302 {
			loc := r.Header.Get("Location")
			r.Body.Close()
			r = ask(t, base, "192.0.2.1", loc)
		}
		text := body(r)
		m := variant.FindStringSubmatch(text)
		if r.StatusCode != 200 || m == nil {
			t.Fatalf("elsewhere %v: the master playlist: %d %q", elsewhere, r.StatusCode, text)
		}
		// The player plays the variant: it asks for that playlist only, about once a target duration, for
		// longer than a session not asked for is the first to make room (hlsIdle).
		mu.Lock()
		for _, s := range sessions {
			s.used -= hlsIdle + 1
		}
		mu.Unlock()
		v := ask(t, base, "192.0.2.1", m[1])
		if text := body(v); v.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
			t.Fatalf("elsewhere %v: the variant: %d %q", elsewhere, v.StatusCode, text)
		}
		film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
		film.Body.Close()
		if film.StatusCode != 503 {
			t.Errorf("elsewhere %v: another device while the only connection plays live HLS: %d", elsewhere, film.StatusCode)
		}
	}
}
