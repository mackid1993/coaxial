package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PHP's json_encode (and Python's json) write every character past ASCII as \uXXXX by default. A provider login
// with such a character is written that way in the login answer, and must still be swapped for the app's own.
func TestLoginAnswerEscapedLogin(t *testing.T) {
	const answer = `{"user_info":{"username":"j\u00fcrgen","password":"p\u00e4ss-w\u00d6rd-\ud83d\udcfa","auth":1},` +
		`"server_info":{"url":"prov.example","port":"80"}}`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "jürgen" || r.URL.Query().Get("password") != "päss-wÖrd-📺" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(answer))
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "jürgen", Password: "päss-wÖrd-📺", Port: "18513", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/player_api.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18513"
	f.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"user_info":{"username":"tv","password":"easy"`) ||
		strings.Contains(body, `u00fcrgen`) || strings.Contains(body, `\u00e4ss`) {
		t.Errorf("%d %q", w.Code, body)
	}
}
