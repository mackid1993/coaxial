package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The guide (xmltv.php) names a picture for each channel and programme. A player fetches them, so they must
// be addresses on this proxy, as in the lists: fetched from the player's home over the open internet, they
// would go around the VPN and show the provider's own server.
func TestGuidePicturesComeBackHere(t *testing.T) {
	const guide = `<?xml version="1.0" encoding="utf-8" ?>` + "\n" + `<tv generator-info-name="Xtream Codes">` + "\n" +
		`<channel id="one.uk"><display-name>One</display-name><icon src="http://%s/images/1.png" /></channel>` + "\n" +
		`<channel id="two.uk"><display-name>Two &amp; more</display-name><icon src="https://logos.example/2.png" /></channel>` + "\n" +
		`<programme start="20261122100000 +0000" stop="20261122110000 +0000" channel="one.uk"><title>News</title>` +
		`<desc>See http://news.example</desc><icon src="http://%s/images/p.jpg" /></programme>` + "\n</tv>\n"
	var prov *httptest.Server
	prov = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xmltv.php" || r.URL.Query().Get("username") != "realU7" || r.URL.Query().Get("password") != "realP7-long" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, strings.ReplaceAll(guide, "%s", prov.Listener.Addr().String()))
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	at := prov.Listener.Addr().String()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU7", Password: "realP7-long", Port: "18507", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/xmltv.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18507"
	f.ServeHTTP(w, r)
	want := strings.NewReplacer(`"http://%s/`, `"http://coaxial.lan:18507/__cdn/http/`+at+`/`,
		`"https://logos.example/`, `"http://coaxial.lan:18507/__cdn/https/logos.example/`).Replace(guide)
	if body := w.Body.String(); w.Code != 200 || body != want {
		t.Errorf("%d, the guide's pictures are not fetched through this proxy:\n%s\nwant:\n%s", w.Code, body, want)
	}
}
