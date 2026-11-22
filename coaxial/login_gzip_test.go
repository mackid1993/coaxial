package main

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider that gzips its login answer although it was asked for plain text (as some send their guide): the
// app is still given the whole answer, unpacked, with its own login and Coaxial's address in it.
func TestLoginAnswerUnaskedGzip(t *testing.T) {
	const answer = `{"user_info":{"username":"realU5","password":"realP5-long","auth":1,"status":"Active"},` +
		`"server_info":{"url":"prov.example","port":"80","https_port":"443","server_protocol":"http"}}`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "realU5" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		z.Write([]byte(answer))
		z.Close()
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU5", Password: "realP5-long", Port: "18511", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/player_api.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18511"
	f.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"user_info":{"username":"tv","password":"easy"`) ||
		!strings.Contains(body, `"url":"coaxial.lan"`) || strings.Contains(body, "realU5") || strings.Contains(body, "prov.example") {
		t.Errorf("%d %q", w.Code, body)
	}
}
