package main

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A player that accepts gzip (most HTTP libraries add it on their own) has that passed on to the provider's own
// server for films, series and catch-up. A server that packs its error pages (Apache with mod_deflate, a CDN in
// front) then sends the page that echoes the address asked for, real login and all, packed: the login swap on
// the page's bytes found nothing to swap, and the app unpacked the real login.
func TestPackedErrorPageKeepsLoginHidden(t *testing.T) {
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := fmt.Sprintf("<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>404 Not Found</title>\n</head><body>\n"+
			"<h1>Not Found</h1>\n<p>The requested URL %s was not found on this server.</p>\n<hr>\n<address>Apache Server at %s Port 80</address>\n</body></html>\n",
			r.URL.RequestURI(), r.Host)
		w.Header().Set("Content-Type", "text/html")
		var z io.WriteCloser
		switch ae := r.Header.Get("Accept-Encoding"); {
		case strings.Contains(ae, "gzip"):
			w.Header().Set("Content-Encoding", "gzip")
			z, _ = gzip.NewWriterLevel(w, gzip.BestCompression) // (packed for real, as Apache's mod_deflate does)
		case strings.Contains(ae, "deflate"): // (an encoding Coaxial does not unpack)
			w.Header().Set("Content-Encoding", "deflate")
			z, _ = zlib.NewWriterLevel(w, zlib.BestCompression)
		}
		w.WriteHeader(404)
		if z == nil {
			io.WriteString(w, page)
			return
		}
		io.WriteString(z, page)
		z.Close()
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	for _, enc := range []string{"gzip", "deflate"} {
		for _, path := range []string{"/movie/tv/easy/12.mkv", "/series/tv/easy/13.mp4", "/timeshift/tv/easy/60/2026-11-22:10-00/14.ts",
			"/streaming/timeshift.php?username=tv&password=easy&stream=17"} {
			f := newFront(provider{Name: "P", URL: prov.URL, User: "realU7", Password: "re@lP7-long", Port: "18509", LiveTV: "ts"},
				player{User: "tv", Password: "easy"})
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", path, nil)
			r.RemoteAddr = "192.0.2.10:5000"
			r.Header.Set("Accept-Encoding", enc)
			f.ServeHTTP(w, r)
			body := w.Body.String() // as the app reads it
			var z io.Reader
			switch w.Header().Get("Content-Encoding") {
			case "gzip":
				z, _ = gzip.NewReader(strings.NewReader(body))
			case "deflate":
				z, _ = zlib.NewReader(strings.NewReader(body))
			}
			if z != nil {
				b, err := io.ReadAll(z)
				if err != nil {
					t.Errorf("%s (%s): the page cannot be unpacked: %v", path, enc, err)
				}
				body = string(b)
			}
			if w.Code != 404 || enc == "gzip" && !strings.Contains(body, "was not found") {
				t.Errorf("%s (%s): %d %q", path, enc, w.Code, body)
			}
			if strings.Contains(body, "realU7") || strings.Contains(body, "re@lP7") || strings.Contains(body, "re%40lP7") {
				t.Errorf("%s (%s): the provider's packed error page gives the player the real login: %q", path, enc, body)
			}
		}
	}
}
