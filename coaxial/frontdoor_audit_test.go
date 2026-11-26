package main

// Front-door audit (lists and guide): what a player straight at the provider gets, compared with what it gets
// through Coaxial. Each test talks to a fake Xtream provider that answers the way the panels do (XUI and Xtream
// Codes read username, password and action from the query or from a POST form alike, and a static guide file
// behind xmltv.php answers conditional requests with 304).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// auditFront: a front door for prov served on a real port (the Go server's own rules on bodies apply), with
// every request coming from one device.
func auditFront(t *testing.T, prov *httptest.Server, p string) string {
	t.Helper()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU8", Password: "realP8-long", Port: port(p), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "192.0.2.10:5000"
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// rawClient: a player's HTTP client that sends its own Accept-Encoding (as URLSession, ExoPlayer's OkHttp and
// curl do) and does not unpack or follow anything itself.
var rawClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// 1. A guide (or list) the player already has: the player asks again with If-None-Match (URLSession does this
// on its own for a cached answer that carried an ETag), and the provider answers 304 Not Modified. Straight at
// the provider the player gets the 304 and keeps its copy. Through Coaxial, a player that also sent
// Accept-Encoding: gzip gets its connection cut with no answer at all.
func TestAuditNotModifiedWithGzip(t *testing.T) {
	const guide = `<tv><channel id="one"><icon src="https://logos.example/1.png" /></channel></tv>`
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "realU8" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("ETag", `"g1"`)
		w.Header().Set("Content-Type", "application/xml")
		if r.Header.Get("If-None-Match") == `"g1"` {
			w.WriteHeader(304)
			return
		}
		io.WriteString(w, guide)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	base := auditFront(t, prov, "18711")
	for _, path := range []string{"/xmltv.php?username=tv&password=easy", "/player_api.php?username=tv&password=easy&action=get_live_categories"} {
		for _, enc := range []string{"", "gzip, deflate, br"} {
			req, _ := http.NewRequest("GET", base+path, nil)
			req.Header.Set("If-None-Match", `"g1"`)
			if enc != "" {
				req.Header.Set("Accept-Encoding", enc)
			}
			resp, err := rawClient.Do(req)
			if err != nil {
				t.Errorf("%s, Accept-Encoding %q: no answer at all (%v); straight at the provider: 304 Not Modified", path, enc, err)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 304 || len(b) != 0 {
				t.Errorf("%s, Accept-Encoding %q: %d with %d bytes, want 304 and nothing", path, enc, resp.StatusCode, len(b))
			}
			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Errorf("%s, Accept-Encoding %q: a 304 marked Content-Encoding %q", path, enc, ce)
			}
		}
	}
}

// 2. An app that logs in and reads its lists with POST (username, password and action in a form body, which
// Xtream panels read exactly as the query). Straight at the provider: logged in. Through Coaxial the provider is
// asked GET /player_api.php with no login at all, and the app is told auth 0.
func TestAuditAPIPostForm(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() // (query and POST form together, as PHP's $_REQUEST)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" user="+r.Form.Get("username"))
		mu.Unlock()
		if r.Form.Get("username") != "realU8" || r.Form.Get("password") != "realP8-long" {
			io.WriteString(w, `{"user_info":{"auth":0}}`)
			return
		}
		if r.Form.Get("action") == "get_live_categories" {
			io.WriteString(w, `[{"category_id":"1","category_name":"News","parent_id":0}]`)
			return
		}
		io.WriteString(w, `{"user_info":{"username":"realU8","password":"realP8-long","auth":1,"status":"Active"},`+
			`"server_info":{"url":"prov.example","port":"8080","https_port":"8443","server_protocol":"http"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	base := auditFront(t, prov, "18712")
	for _, c := range []struct{ form, want string }{
		{"username=tv&password=easy", `"auth":1`},
		{"username=tv&password=easy&action=get_live_categories", `"category_name":"News"`},
	} {
		resp, err := rawClient.PostForm(base+"/player_api.php", mustForm(c.form))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), c.want) {
			mu.Lock()
			t.Errorf("POST %s: the app got %d %s, want %s (straight at the provider it would); the provider saw %q",
				c.form, resp.StatusCode, b, c.want, seen)
			mu.Unlock()
		}
		if strings.Contains(string(b), "realU8") || strings.Contains(string(b), "realP8") {
			t.Errorf("POST %s: the provider's login reached the app: %s", c.form, b)
		}
	}
}

func mustForm(s string) url.Values { v, _ := url.ParseQuery(s); return v }

// 3. A HEAD for the guide (a client checking its size or date before it downloads). Straight at the provider:
// one HEAD, headers only. Through Coaxial the provider is sent a GET, and the whole guide is downloaded through
// the VPN and thrown away.
func TestAuditGuideHead(t *testing.T) {
	var mu sync.Mutex
	methods, sent := []string{}, 0
	big := strings.Repeat(`<programme channel="one"><title>x</title></programme>`, 20000) // (about 1 MB)
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		n, _ := io.WriteString(w, "<tv>"+big+"</tv>")
		mu.Lock()
		if r.Method == "GET" {
			sent += n
		}
		mu.Unlock()
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	base := auditFront(t, prov, "18713")
	req, _ := http.NewRequest("HEAD", base+"/xmltv.php?username=tv&password=easy", nil)
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != "HEAD" {
		t.Errorf("a player's HEAD reached the provider as %q (%d bytes of guide downloaded and dropped); want one HEAD", methods, sent)
	}
}
