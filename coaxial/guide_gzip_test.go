package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider that sends its guide gzipped although it was asked for plain text: it is unpacked and rewritten
// like any other answer, not refused.
func TestGuideUnaskedGzip(t *testing.T) {
	const guide = `<tv><channel id="one"><icon src="https://logos.example/1.png" /></channel></tv>`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xmltv.php" || r.URL.Query().Get("username") != "realU8" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		io.WriteString(z, guide)
		z.Close()
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU8", Password: "realP8-long", Port: "18508", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/xmltv.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18508"
	f.ServeHTTP(w, r)
	want := strings.Replace(guide, `"https://logos.example/`, `"http://coaxial.lan:18508/__cdn/https/logos.example/`, 1)
	if body := w.Body.String(); w.Code != 200 || body != want || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("%d %q, %q; want 200 and %q, not packed", w.Code, w.Header().Get("Content-Encoding"), body, want)
	}
}
