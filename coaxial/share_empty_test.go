package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// An empty answer (status 200, nothing in it: a provider's passing hiccup) is not kept: the next player asks again.
func TestShareEmptyNotKept(t *testing.T) {
	var mu sync.Mutex
	n := 0
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/xmltv.php" {
			return false
		}
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if !first {
			io.WriteString(w, `<tv></tv>`)
		}
		return true
	})
	f := newFront(provider{Name: "E", URL: fp.srv.URL, User: "u", Password: "p", Port: "18624", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	var g string
	for i := 0; i < 2; i++ {
		r, err := devClient.Get(srv.URL + "/xmltv.php?username=tv&password=easy")
		if err != nil {
			t.Fatal(err)
		}
		g = body(r)
	}
	if g != "<tv></tv>" {
		t.Errorf("an empty answer was kept: %q", g)
	}
}
