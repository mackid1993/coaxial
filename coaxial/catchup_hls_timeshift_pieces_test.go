package main

import (
	"net/http"
	"strings"
	"testing"
)

// Catch-up as HLS whose pieces are themselves catch-up addresses on the provider's own server
// (/timeshift/<login>/<minutes>/<start>/<channel>.ts, or timeshift.php), with the playlist sent elsewhere on
// the provider's server or answered directly. A piece is part of the catch-up the device plays, not a new
// catch-up of the channel: the session goes on while its pieces are read, and another device opening the
// channel meanwhile is refused.
func TestCatchUpHLSTimeshiftPieces(t *testing.T) {
	list := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n" +
		"#EXTINF:10,\n/timeshift/realU/realP/1/2026-01-01:10-00/12.ts\n" +
		"#EXTINF:10,\n/streaming/timeshift.php?username=realU&password=realP&stream=12&start=2026-01-01:10-01&duration=1\n" +
		"#EXTINF:10,\n/timeshift/realU/realP/1/2026-01-01:10-02/12.ts\n#EXT-X-ENDLIST\n"
	for _, c := range []struct {
		direct bool
		mode   string
	}{{false, "ts"}, {true, "ts"}, {false, "hls"}, {true, "hls"}} {
		direct := c.direct
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == "/timeshift/realU/realP/60/2026-01-01:10-00/12.m3u8" && !direct:
				http.Redirect(w, r, "/hlsr/tok12/realU/realP/12/index.m3u8", 302)
			case strings.HasSuffix(r.URL.Path, ".m3u8"):
				w.Write([]byte(list))
			case r.URL.Path == "/timeshift/realU/realP/1/2026-01-01:10-00/12.ts" || r.URL.Path == "/timeshift/realU/realP/1/2026-01-01:10-02/12.ts" ||
				r.URL.Path == "/streaming/timeshift.php" && r.URL.Query().Get("username") == "realU":
				w.Header().Set("Content-Type", "video/mp2t")
				w.Write(make([]byte, 188*50))
			default:
				return false
			}
			return true
		})
		f, base := advFront(t, fp, c.mode, advanced{Connections: "2", OnePerChannel: true})
		r := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8")
		for r.StatusCode == 302 {
			loc := r.Header.Get("Location")
			r.Body.Close()
			r = ask(t, base, "192.0.2.1", loc)
		}
		text := body(r)
		var pieces []string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "/") {
				pieces = append(pieces, line)
			}
		}
		if r.StatusCode != 200 || len(pieces) != 3 || strings.Contains(text, "realU") {
			t.Fatalf("direct %v, %s: the catch-up playlist: %d %q", direct, c.mode, r.StatusCode, text)
		}
		for i, piece := range pieces {
			if p := ask(t, base, "192.0.2.1", piece); p.StatusCode != 200 {
				t.Fatalf("direct %v, %s: piece %d: %d", direct, c.mode, i, p.StatusCode)
			} else {
				p.Body.Close()
			}
			mu.Lock()
			n := 0
			for k, s := range sessions {
				if strings.HasPrefix(k, f.base+" ") {
					n++
					s.used -= 20 // (the piece plays for a while: the playlist is not asked for again)
					for d := range s.who {
						s.who[d] -= 20
					}
				}
			}
			mu.Unlock()
			if n == 0 {
				t.Errorf("direct %v, %s: after piece %d the catch-up no longer counts at the provider", direct, c.mode, i)
			}
			other := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
			other.Body.Close()
			if other.StatusCode != 503 {
				t.Errorf("direct %v, %s: after piece %d another device opened the channel while its catch-up plays: %d", direct, c.mode, i, other.StatusCode)
			}
		}
	}
}
