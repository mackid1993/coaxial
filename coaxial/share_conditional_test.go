package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The player that starts a download asked "only if changed" (304): a player asking plainly meanwhile is given the
// whole guide, not a 304 it cannot use (it was cut off).
func TestShareNoConditionalRequest(t *testing.T) {
	slow := make(chan struct{})
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/xmltv.php" {
			return false
		}
		<-slow
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(304)
			return true
		}
		io.WriteString(w, `<tv></tv>`)
		return true
	})
	f := newFront(provider{Name: "C", URL: fp.srv.URL, User: "u", Password: "p", Port: "18625", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	guide := srv.URL + "/xmltv.php?username=tv&password=easy"
	first := make(chan struct{})
	go func() {
		defer close(first)
		req, _ := http.NewRequest("GET", guide, nil)
		req.Header.Set("If-Modified-Since", "Sat, 21 Nov 2026 10:00:00 GMT")
		if r, err := devClient.Do(req); err == nil {
			body(r)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	plain := make(chan string, 1)
	go func() {
		r, err := devClient.Get(guide)
		if err != nil {
			plain <- err.Error()
			return
		}
		plain <- r.Status + " " + body(r)
	}()
	time.Sleep(200 * time.Millisecond)
	close(slow)
	if g := <-plain; g != "200 OK <tv></tv>" {
		t.Errorf("a plain request was answered %q", g)
	}
	<-first
}
