package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A live HLS session nobody asked for in its held time no longer counts (the player paused, or the screen went
// to sleep). At an account that allows 5, other devices take all five connections meanwhile. The device coming
// back to the session is a new connection and is refused, so the provider never sees six.
func TestLimitHLSSessionComingBack(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/944.m3u8" {
			http.Redirect(w, r, "/hlsr/tok944/realU/realP/944/index.m3u8", 302)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n944_1.ts\n")
			return true
		}
		return false
	})
	f, base := advFront(t, fp, "hls", advanced{Connections: "5"})
	r := ask(t, base, "192.0.2.1", "/live/tv/easy/944.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("live HLS: %d %q", r.StatusCode, text)
	}
	mu.Lock() // the player stops asking for longer than the session's held time (the same as waiting it out)
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= s.held() + 1
			for d := range s.who {
				s.who[d] -= s.held() + 1
			}
		}
	}
	mu.Unlock()
	for i, id := range []string{"921", "922", "923", "924", "925"} {
		film := ask(t, base, "192.0.2."+string(rune('2'+i)), "/movie/tv/easy/"+id+".mkv")
		if film.StatusCode != 200 {
			film.Body.Close()
			t.Fatalf("device %d once the session no longer counts: %d", i+2, film.StatusCode)
		}
		defer film.Body.Close()
	}
	before := fp.count(fp.hits, "/hlsr/tok944/realU/realP/944/index.m3u8")
	back := ask(t, base, "192.0.2.1", "/__ch/live/tv/easy/944.m3u8/__index.m3u8")
	if text := body(back); back.StatusCode != 503 {
		t.Errorf("the first device coming back while all five connections are in use: %d %q", back.StatusCode, text)
	}
	if fp.count(fp.hits, "/hlsr/tok944/realU/realP/944/index.m3u8") != before {
		t.Errorf("the provider was asked for the session over the limit")
	}
	if n := fp.count(fp.most, ""); n > 5 {
		t.Errorf("the provider saw %d streams at once, the account allows 5", n)
	}
}
