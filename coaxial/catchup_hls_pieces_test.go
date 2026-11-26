package main

import (
	"net/http"
	"strings"
	"testing"
)

// Catch-up as HLS: the playlist is asked for once (it is complete) and the pieces come from the server the
// provider sent the player to. The session is in use for as long as its pieces are asked for, not only 30 s
// after the playlist: another device opening the channel meanwhile is refused.
func TestOnePerChannelCatchUpHLSPieces(t *testing.T) {
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			w.Write([]byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nseg1.ts\n#EXTINF:10,\nseg2.ts\n#EXT-X-ENDLIST\n"))
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 188*50))
		}
		return true
	})
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") {
			http.Redirect(w, r, cdn.srv.URL+"/arch/tok12/index.m3u8", 302)
			return true
		}
		return false
	})
	f, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	r := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	text := body(r)
	if r.StatusCode != 200 || !strings.Contains(text, "/__cdn/http/") {
		t.Fatalf("the catch-up playlist: %d %q", r.StatusCode, text)
	}
	var pieces []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "/__cdn/"); i >= 0 && !strings.HasPrefix(line, "#") {
			pieces = append(pieces, line[i:]) // (named in full, at this proxy's address: the path is asked for)
		}
	}
	if p := ask(t, base, "192.0.2.1", pieces[0]); p.StatusCode != 200 {
		t.Fatalf("the first piece: %d", p.StatusCode)
	} else {
		p.Body.Close()
	}
	mu.Lock() // half a minute of playing later: the playlist was not asked for again (it is complete)...
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= 31
			for d := range s.who {
				s.who[d] -= 31
			}
		}
	}
	for h := range holds { // ...and the place kept for the redirect's follow-up (10 s) is long over
		if h.until > 0 {
			h.until -= 31
		}
	}
	mu.Unlock()
	if p := ask(t, base, "192.0.2.1", pieces[1]); p.StatusCode != 200 { // ...and the player is on the next piece
		t.Fatalf("the next piece: %d", p.StatusCode)
	} else {
		p.Body.Close()
	}
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
	rec.Body.Close()
	if rec.StatusCode != 503 {
		t.Errorf("another device opened the channel while its catch-up plays as HLS: %d", rec.StatusCode)
	}
}
