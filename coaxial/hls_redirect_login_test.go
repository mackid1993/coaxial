package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// An HLS film, episode, live channel or catch-up whose address the provider answers with a redirect to a
// playlist on its own server. The player is given the channel's folder (/__ch/...), and what it asks for there
// goes to the provider with the provider's login, never the player's; nothing the player is given (playlist,
// Location) carries the provider's login.
func TestHLSRedirectKeepsLogins(t *testing.T) {
	for _, path := range []string{
		"/movie/realU/realP/7.m3u8",
		"/series/realU/realP/7.m3u8",
		"/live/realU/realP/7.m3u8",
		"/timeshift/realU/realP/60/2026-01-01:10-00/7.m3u8",
	} {
		var asked []string
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			fp.mu.Lock()
			asked = append(asked, r.URL.RequestURI())
			fp.mu.Unlock()
			switch {
			case r.URL.Path == path:
				http.Redirect(w, r, "/hlsr/tok7/realU/realP/7/index.m3u8", 302)
			case r.URL.Path == "/hlsr/tok7/realU/realP/7/index.m3u8":
				io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\npiece1.ts\n#EXT-X-ENDLIST\n")
			case r.URL.Path == "/hlsr/tok7/realU/realP/7/piece1.ts":
				w.Header().Set("Content-Type", "video/mp2t")
				w.Write(make([]byte, 188*50))
			default:
				http.NotFound(w, r)
			}
			return true
		})
		_, base := advFront(t, fp, "hls", advanced{StayUnder: off()})
		checkAsked := func() {
			fp.mu.Lock()
			defer fp.mu.Unlock()
			for _, a := range asked {
				if strings.Contains(a, "/tv/") || strings.Contains(a, "easy") {
					t.Errorf("%s: the provider was asked with the player's login: %s", path, a)
				}
			}
			if n := fp.hits[path]; n != 1 {
				t.Errorf("%s: the provider was asked to open it %d times", path, n)
			}
		}
		var given []string // everything the player was given
		r := ask(t, base, "192.0.2.1", strings.Replace(path, "realU/realP", "tv/easy", 1))
		for r.StatusCode == 302 {
			loc := r.Header.Get("Location")
			given = append(given, loc)
			r.Body.Close()
			r = ask(t, base, "192.0.2.1", loc)
		}
		text := body(r)
		given = append(given, text)
		if r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
			t.Errorf("%s: the playlist: %d %q", path, r.StatusCode, text)
			checkAsked()
			continue
		}
		var piece string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "/") {
				piece = line
			}
		}
		if p := ask(t, base, "192.0.2.1", piece); p.StatusCode != 200 {
			t.Errorf("%s: the piece %q: %d", path, piece, p.StatusCode)
			p.Body.Close()
		} else {
			p.Body.Close()
		}
		// (a player that reads the piece next to the channel's folder instead)
		folder := "/__ch" + strings.Replace(path, "realU/realP", "tv/easy", 1) + "/piece1.ts"
		n := ask(t, base, "192.0.2.1", folder)
		given = append(given, n.Header.Get("Location"))
		n.Body.Close()
		if n.StatusCode != 302 {
			t.Errorf("%s: the piece next to the folder: %d", path, n.StatusCode)
		} else if p := ask(t, base, "192.0.2.1", n.Header.Get("Location")); p.StatusCode != 200 {
			t.Errorf("%s: the piece next to the folder, where it was sent: %d", path, p.StatusCode)
			p.Body.Close()
		} else {
			p.Body.Close()
		}
		for _, g := range given {
			if strings.Contains(g, "realU") || strings.Contains(g, "realP") {
				t.Errorf("%s: the player was given the provider's login: %q", path, g)
			}
		}
		checkAsked()
	}
}
