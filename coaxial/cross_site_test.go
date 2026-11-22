package main

import (
	"net"
	"net/http"
	"strconv"
	"testing"
)

// Another web site open in a browser on the same network must not be able to press the page's buttons.
func TestCrossSitePosts(t *testing.T) {
	probe, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	n := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	l := &launcher{quit: make(chan int, 4)}
	if err := l.listenUI(n); err != nil {
		t.Fatal(err)
	}
	defer l.ui.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(n)
	post := func(path string, h map[string]string) int {
		req, _ := http.NewRequest("POST", base+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	try := "/api/vpn/server/pick" // (with nothing set up, it only answers)
	if got := post(try, map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}); got != 403 {
		t.Errorf("a post from another site: %d, want 403", got)
	}
	if got := post(try, map[string]string{"Origin": "null"}); got != 403 {
		t.Errorf("a post from a page with no origin: %d, want 403", got)
	}
	if got := post(try, map[string]string{"Origin": base, "Sec-Fetch-Site": "same-origin"}); got != 200 {
		t.Errorf("a post from the page itself: %d, want 200", got)
	}
	if got := post(try, nil); got != 200 { // (curl and the like)
		t.Errorf("a post with no origin: %d, want 200", got)
	}
	r, err := http.Get(base + "/api/status")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("reading the status: %v %v", r, err)
	}
	r.Body.Close()
}
