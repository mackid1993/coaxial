package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Some servers write the address asked for into their error pages ("The requested URL ... was not found").
// Coaxial passes a provider's error answers on, so the address it asked for, with the real login in it, must
// not reach the player there: films and the like, live MPEG-TS, HLS playlists, HLS built from MPEG-TS.
func TestErrorPageKeepsLoginHidden(t *testing.T) {
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		fmt.Fprintf(w, "<h1>Not Found</h1><p>The requested URL %s was not found on this server.</p>", r.URL.RequestURI())
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	for _, c := range []struct{ mode, path string }{
		{"ts", "/movie/tv/easy/12.mkv"},
		{"ts", "/series/tv/easy/13.mp4"},
		{"ts", "/live/tv/easy/14.ts"},
		{"ts", "/live/tv/easy/15.m3u8"},
		{"hls", "/live/tv/easy/16.m3u8"},
		{"ts", "/streaming/timeshift.php?username=tv&password=easy&stream=17"},
	} {
		f := newFront(provider{Name: "P", URL: prov.URL, User: "realU6", Password: "re@lP6-long", Port: "18508", LiveTV: c.mode},
			player{User: "tv", Password: "easy"})
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", c.path, nil)
		r.RemoteAddr = "192.0.2.10:5000"
		f.ServeHTTP(w, r)
		if w.Code == 302 { // (a live HLS channel: its folder)
			r = httptest.NewRequest("GET", w.Header().Get("Location"), nil)
			r.RemoteAddr, w = "192.0.2.10:5000", httptest.NewRecorder()
			f.ServeHTTP(w, r)
		}
		body := w.Body.String()
		if w.Code != 404 || !strings.Contains(body, "was not found") {
			t.Errorf("%s (%s): %d %q", c.path, c.mode, w.Code, body)
		}
		if strings.Contains(body, "realU6") || strings.Contains(body, "re@lP6") || strings.Contains(body, "re%40lP6") {
			t.Errorf("%s (%s): the provider's error page gives the player the real login: %q", c.path, c.mode, body)
		}
	}
}
