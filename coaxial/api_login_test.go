package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The answer to an app's login (player_api.php with no action) carries the account in user_info, as every
// Xtream server sends it. The app must be given its own login there, never the provider's, whether the answer
// is read whole or arrives in small pieces.
func TestAPIKeepsLoginHidden(t *testing.T) {
	const answer = `{"user_info":{"username":"realUser77","password":"realPassw0rd-long-enough","message":"","auth":1,` +
		`"status":"Active","exp_date":"1798761600","active_cons":"0","max_connections":"2"},` +
		`"server_info":{"url":"prov.example","port":"80","https_port":"443","server_protocol":"http"},` +
		`"logo":"http:\/\/prov.example\/logo.png","id":"realPassw0rd-long-enough"}`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "realUser77" || r.URL.Query().Get("password") != "realPassw0rd-long-enough" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < len(answer); i += 7 { // (in small pieces, as a slow server sends it)
			io.WriteString(w, answer[i:min(len(answer), i+7)])
			w.(http.Flusher).Flush()
		}
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realUser77", Password: "realPassw0rd-long-enough", Port: "18504", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/player_api.php?username=tv&password=easy", nil)
	r.RemoteAddr, r.Host = "192.0.2.10:5000", "coaxial.lan:18504"
	f.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"user_info":{"username":"tv","password":"easy","message"`) {
		t.Fatalf("%d %s", w.Code, body)
	}
	if strings.Contains(body, "realUser77") || strings.Contains(body, `"password":"realPassw0rd`) {
		t.Errorf("the provider's login reaches the app:\n%s", body)
	}
	if !strings.Contains(body, `"http://coaxial.lan:18504/__cdn/http/prov.example\/logo.png"`) || !strings.Contains(body, `"id":"realPassw0rd-long-enough"`) {
		t.Errorf("links or other values changed:\n%s", body)
	}
	// read whole, and read in pieces of every size from 1 to 40 bytes: the same answer
	login := [][2]string{{`"password"\s{0,4}:\s{0,4}"realPassw0rd-long-enough"`, `"password":"easy"`}}
	var whole bytes.Buffer
	apiStream(&whole, strings.NewReader(answer), []byte(`"http://me/__cdn/`), login, false)
	for step := 1; step <= 40; step++ {
		var parts bytes.Buffer
		apiStream(&parts, &smallReads{[]byte(answer), step}, []byte(`"http://me/__cdn/`), login, false)
		if parts.String() != whole.String() || strings.Contains(parts.String(), `"password":"realP`) {
			t.Fatalf("read in pieces of %d:\n%s\nwhole:\n%s", step, parts.String(), whole.String())
		}
	}
}
