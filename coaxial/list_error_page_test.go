package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An error page from the provider's lists or guide (a login refused, a banned line) that names the account is
// given to the app with its own login in it, as on every other path (see hideLogin).
func TestListErrorPageKeepsLoginHidden(t *testing.T) {
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		w.Write([]byte("<h1>403</h1>Line realU6 is banned. Asked: " + r.URL.RawQuery))
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU6", Password: "realP6-long", Port: "18512", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	for _, target := range []string{"/player_api.php?username=tv&password=easy", "/player_api.php?username=tv&password=easy&action=get_live_streams",
		"/panel_api.php?username=tv&password=easy", "/xmltv.php?username=tv&password=easy"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18512"
		f.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 403 || !strings.Contains(body, "Line tv is banned") || strings.Contains(body, "realU6") || strings.Contains(body, "realP6") {
			t.Errorf("%s: %d %q", target, w.Code, body)
		}
	}
}
