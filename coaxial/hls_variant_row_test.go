package main

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// A live HLS channel whose playlist is a list of variants: the player asks for the channel's own playlist once
// and from then on only for the variant's. The device is still playing: its row in the status list (which is
// also what counts it as playing for background work, see playingNow) must go on for as long as the variant is
// asked for, not end 30 s after the tune.
func TestHLSVariantKeepsStatusRow(t *testing.T) {
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
	for _, elsewhere := range []bool{true, false} { // the variant on another server, and on the provider's own
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
		f, base := limFront(t, fp, "hls", "", advanced{})
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
		if n := rowsOf45(f); n != 1 {
			t.Fatalf("elsewhere %v: %d rows right after the tune, want 1", elsewhere, n)
		}
		for i := 0; i < 4; i++ { // 40 s of playing: the variant asked for every 10 s
			mu.Lock()
			for _, row := range hlsRows {
				if row.provider == f.base {
					row.last -= 10
				}
			}
			mu.Unlock()
			v := ask(t, base, "192.0.2.1", m[1])
			if text := body(v); v.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
				t.Fatalf("elsewhere %v: the variant: %d %q", elsewhere, v.StatusCode, text)
			}
		}
		if n := rowsOf45(f); n != 1 {
			t.Errorf("elsewhere %v: after 40 s of playing the variant the status list has %d rows for the device, want 1", elsewhere, n)
		}
	}
}

func rowsOf45(f *front) (n int) {
	mu.Lock()
	defer mu.Unlock()
	for _, row := range hlsRows {
		if row.who == "192.0.2.1" && row.provider == f.base && row.id == "45" && mono()-row.last <= 30 {
			n++
		}
	}
	return n
}
