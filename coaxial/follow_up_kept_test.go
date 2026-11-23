package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// At the limit, the place of a film answered with a redirect is kept for its device's request to the server it
// was sent to: another device asking in between is turned away (it would put a second stream at the provider),
// the follow-up takes the place, and a place nobody comes back for is let go after a while.
func TestFollowUpKeepsItsPlace(t *testing.T) {
	cdn := newFakeProv(t, true, nil)
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/movie/realU/realP/5.") {
			http.Redirect(w, r, cdn.srv.URL+"/movie/tok/5.mkv?t=abc", 302)
			return true
		}
		return false
	})
	f, base := advFront(t, fp, "ts", advanced{Connections: "1"})
	r := ask(t, base, "192.0.2.1", "/movie/tv/easy/5.mkv")
	loc := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/http/") {
		t.Fatalf("the redirect: %d %q", r.StatusCode, loc)
	}
	other := ask(t, base, "192.0.2.2", "/movie/tv/easy/6.mkv") // (between the redirect and its follow-up)
	if other.StatusCode != 503 {
		t.Errorf("another device took the place of a film sent on: %d", other.StatusCode)
	}
	defer other.Body.Close()
	follow := ask(t, base, "192.0.2.1", loc)
	if follow.StatusCode != 200 {
		t.Fatalf("the follow-up: %d %q", follow.StatusCode, body(follow))
	}
	if a, b := fp.count(fp.open, ""), cdn.count(cdn.open, ""); a+b != 1 {
		t.Errorf("the provider and the server it sent to have %d and %d streams open, want 1 in all", a, b)
	}
	other.Body.Close()
	mu.Lock()
	n := len(f.conns())
	mu.Unlock()
	if n != 1 {
		t.Errorf("the follow-up and its kept place count as %d connections, want 1", n)
	}
	follow.Body.Close()
	eventually(t, func() bool { return cdn.count(cdn.open, "") == 0 }, "the film closing")

	// Nobody comes back for it: the place is let go.
	mu.Lock()
	was := followUpHold
	followUpHold = 0.3
	mu.Unlock()
	defer func() { mu.Lock(); followUpHold = was; mu.Unlock() }()
	r = ask(t, base, "192.0.2.1", "/movie/tv/easy/5.mkv")
	r.Body.Close()
	time.Sleep(500 * time.Millisecond)
	late := ask(t, base, "192.0.2.2", "/movie/tv/easy/6.mkv")
	if late.StatusCode != 200 {
		t.Errorf("a place kept for a follow-up that never came was not let go: %d", late.StatusCode)
	}
	late.Body.Close()
}
