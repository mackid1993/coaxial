package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// The login answer's server_info names a server, and apps build stream addresses from it (some also warn
// when it differs from the address typed in). It must name the address the player used, over http, never
// the provider's server: plain or gzip, whole or in small pieces, a port written as text or as a number.
// Lists (an action asked for) pass as before.
func TestLoginAnswerNamesPlayersAddress(t *testing.T) {
	info := `"server_info":{"xui":true,"url":"prov.example","port":%s,"https_port":"8443","server_protocol":"https",` +
		`"rtmp_port":"8880","timezone":"Europe\/London","timestamp_now":1795361215,"process":true}`
	login := `{"user_info":{"username":"realU9","password":"realP9-long","auth":1,"status":"Active"},` + info + `}`
	list := `[{"num":1,"stream_icon":"http:\/\/prov.example\/1.png",` + info + `}]`
	port := `"8080"`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := login
		if r.URL.Query().Get("action") != "" {
			answer = list
		}
		answer = strings.Replace(answer, "%s", port, 1)
		for i := 0; i < len(answer); i += 7 { // (in small pieces, as a slow server sends it)
			io.WriteString(w, answer[i:min(len(answer), i+7)])
			w.(http.Flusher).Flush()
		}
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU9", Password: "realP9-long", Port: "18505", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	ask := func(target, host string, gz bool) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr, r.Host = "192.0.2.10:5000", host
		if gz {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		f.ServeHTTP(w, r)
		body := w.Body.String()
		if gz {
			z, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatalf("%s: %v", target, err)
			}
			b, _ := io.ReadAll(z)
			body = string(b)
		}
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", target, w.Code, body)
		}
		return body
	}
	for _, c := range []struct {
		host, port, wantURL, wantPort string
		gz                            bool
	}{
		{"coaxial.lan:18505", `"8080"`, `"coaxial.lan"`, `"18505"`, false},
		{"coaxial.lan:18505", `"8080"`, `"coaxial.lan"`, `"18505"`, true},
		{"192.0.2.7:18505", `8080`, `"192.0.2.7"`, `18505`, false},
		{"[2001:db8::7]:18505", `"8080"`, `"[2001:db8::7]"`, `"18505"`, false},
		{"coaxial.lan", `"8080"`, `"coaxial.lan"`, `"80"`, false},
	} {
		port = c.port
		body := ask("/player_api.php?username=tv&password=easy", c.host, c.gz)
		var got struct{ UserInfo, ServerInfo map[string]json.RawMessage }
		if err := json.Unmarshal([]byte(strings.NewReplacer("user_info", "UserInfo", "server_info", "ServerInfo").Replace(body)), &got); err != nil {
			t.Fatalf("%s: %v\n%s", c.host, err, body)
		}
		s := got.ServerInfo
		if strings.Contains(body, "prov.example") || strings.Contains(body, "8443") || string(s["url"]) != c.wantURL ||
			string(s["port"]) != c.wantPort || strings.Trim(string(s["https_port"]), `"`) != strings.Trim(c.wantPort, `"`) ||
			string(s["server_protocol"]) != `"http"` {
			t.Errorf("player came in at %s (gzip %v); the login answer names another server:\n%s", c.host, c.gz, body)
		}
		if string(s["rtmp_port"]) != `"8880"` || string(s["timezone"]) != `"Europe\/London"` && string(s["timezone"]) != `"Europe/London"` ||
			string(s["timestamp_now"]) != "1795361215" || string(s["xui"]) != "true" || string(got.UserInfo["username"]) != `"tv"` {
			t.Errorf("other facts changed:\n%s", body)
		}
	}
	// however the answer is cut, the last piece arriving with the end or not: the same answer
	whole := strings.Replace(login, "%s", `"8080"`, 1)
	var first []byte
	for step := 1; step <= 40; step++ {
		for _, end := range []bool{false, true} {
			var in io.Reader = &smallReads{[]byte(whole), step}
			if end {
				in = iotest.DataErrReader(in)
			}
			out, err := playerServer(in, "coaxial.lan:18505")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(out)
			if first == nil {
				first = b
			}
			if string(b) != string(first) || !strings.Contains(string(b), `"url":"coaxial.lan"`) {
				t.Fatalf("read in pieces of %d (end with the last: %v):\n%s", step, end, b)
			}
		}
	}
	port = `"8080"`
	if body := ask("/player_api.php?username=tv&password=easy&action=get_live_streams", "coaxial.lan:18505", false); body !=
		strings.Replace(strings.Replace(list, "%s", port, 1), `"http:\/\/`, `"http://coaxial.lan:18505/__cdn/http/`, 1) {
		t.Errorf("a list changed beyond its links:\n%s", body)
	}
}
