package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// panel_api.php is the older apps' login (current panels still answer it, like player_api.php). Its answer
// names the account in user_info and the server in server_info, then lists every channel: the player must be
// given its own login and Coaxial's address there, and the channel logos pointed back here.
func TestPanelAPIKeepsLoginHidden(t *testing.T) {
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panel_api.php" || r.URL.Query().Get("username") != "realU8" || r.URL.Query().Get("password") != "realP8-long" {
			io.WriteString(w, `{"user_info":{"auth":0}}`)
			return
		}
		io.WriteString(w, `{"user_info":{"username":"realU8","password":"realP8-long","auth":1,"status":"Active"},`+
			`"server_info":{"url":"prov.example","port":"8080","https_port":"8443","server_protocol":"http","timezone":"UTC"},`+
			`"categories":{"live":[{"category_id":"1","category_name":"News"}]},`+
			`"available_channels":{"1":{"num":1,"name":"One","stream_id":"1","stream_icon":"http:\/\/prov.example:8080\/i\/1.png"}}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU8", Password: "realP8-long", Port: "18506", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/panel_api.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18506"
	f.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, "realU8") || strings.Contains(body, "realP8") || strings.Contains(body, `"url":"prov.example"`) {
		t.Fatalf("the provider's login or server reaches the app: %d\n%s", w.Code, body)
	}
	for _, want := range []string{`"username":"tv","password":"easy"`, `"url":"coaxial.lan"`, `"port":"18506"`,
		`"stream_icon":"http://coaxial.lan:18506/__cdn/http/prov.example:8080\/i\/1.png"`, `"category_name":"News"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}
}
