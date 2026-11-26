package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A provider with a second server sends a film, an episode or catch-up on to it with the account's login in the
// address: Xtream servers answer 302 with the same request on the other server, username= and password=
// included, and some write the login into the path. The player must be given that address with its own (easy)
// login, as for an address on the provider's first server; what it then asks for must reach the second server
// with the provider's login, seeking included. A server the provider never named is never sent the provider's
// login, whatever a player asks.
func TestRedirectToAnotherServerKeepsLoginHidden(t *testing.T) {
	film := strings.Repeat("0123456789", 1000)
	second := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		fp.mu.Lock()
		fp.agents["last"] = r.URL.RequestURI()
		fp.mu.Unlock()
		q := r.URL.Query()
		if q.Get("username") == "realU" && q.Get("password") == "realP" || strings.Contains(r.URL.Path, "/realU/realP/") {
			w.Header().Set("Content-Type", "video/x-matroska")
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(film))
		} else {
			w.WriteHeader(403)
		}
		return true
	})
	stranger := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		fp.mu.Lock()
		fp.agents["last"] = r.URL.RequestURI()
		fp.mu.Unlock()
		w.WriteHeader(404)
		return true
	})
	sent := map[string]string{ // what the provider is asked -> where on its second server it sends the player
		"/movie/realU/realP/5.mkv":                        "/streaming/clients_movie.php?username=realU&password=realP&stream=5.mkv&type=movie&hash=0a1b2c&time=1795361215&pid=77&external_device=",
		"/series/realU/realP/6.mkv":                       "/series/realU/realP/6.mkv?token=t6",
		"/timeshift/realU/realP/60/2026-01-01:10-00/7.ts": "/streaming/timeshift.php?username=realU&password=realP&stream=7&extension=ts&duration=60&start=2026-01-01:10-00&hash=0a1b2c",
	}
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if to := sent[r.URL.Path]; to != "" {
			http.Redirect(w, r, second.srv.URL+to, 302)
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off()})
	last := func(p *fakeProv) string {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.agents["last"]
	}
	for asked, to := range sent {
		r := ask(t, base, "192.0.2.1", strings.Replace(asked, "realU/realP", "tv/easy", 1))
		loc := r.Header.Get("Location")
		r.Body.Close()
		if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/") {
			t.Errorf("%s: %d, Location %q; want a 302 back to this proxy", asked, r.StatusCode, loc)
			continue
		}
		if strings.Contains(loc, "realU") || strings.Contains(loc, "realP") {
			t.Errorf("%s: the player was given the provider's login: Location %q", asked, loc)
		}
		// the player follows, as it does when it seeks: the second server is asked what the provider named
		part := ask(t, base, "192.0.2.1", loc, "Range", "bytes=10-19")
		if text := body(part); part.StatusCode != 206 || text != film[10:20] {
			t.Errorf("%s: following %q: %d %q, want 206 and the part asked for", asked, loc, part.StatusCode, text)
		}
		if got := last(second); got != to {
			t.Errorf("%s: the second server was asked %q, want %q", asked, got, to)
		}
	}
	// a server the provider never sent anyone to: the address goes on as the player wrote it
	host := strings.TrimPrefix(stranger.srv.URL, "http://")
	for _, p := range []string{"/movie/tv/easy/5.mkv", "/streaming/clients_movie.php?username=tv&password=easy&stream=5.mkv"} {
		r := ask(t, base, "192.0.2.1", "/__cdn/http/"+host+p)
		r.Body.Close()
		if got := last(stranger); got != p {
			t.Errorf("a server the provider never named was asked %q, want %q as the player wrote it (never the provider's login)", got, p)
		}
	}
}
