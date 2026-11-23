package main

import (
	"net/http"
	"strings"
	"testing"
)

// The account is locked to one address. The VPN drops and comes back on another address: every stream broke,
// and each player asks again for what it had (a live channel, a film it resumes with a Range request, a film
// the provider had sent on to another server, resumed at that address). Each is a new connection from the new
// address while the provider may still hold the old one, so each waits: the player is told when to try again
// and the provider is not asked. Once the wait is over, they play.
func TestLockWaitAfterAddressChange(t *testing.T) {
	cdn := newFakeProv(t, true, nil)
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/movie/realU/realP/915.") {
			http.Redirect(w, r, cdn.srv.URL+"/movie/tok/915.mkv?t=abc", 302)
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "ts", advanced{Locked: true})
	mu.Lock()
	was := vpnChanged
	vpnChanged = mono() - 1000 // (the VPN has been up a long time)
	mu.Unlock()
	defer func() { mu.Lock(); vpnChanged = was; mu.Unlock() }()

	live := ask(t, base, "192.0.2.1", "/live/tv/easy/912.ts")
	film := ask(t, base, "192.0.2.2", "/movie/tv/easy/911.mkv")
	r := ask(t, base, "192.0.2.3", "/movie/tv/easy/915.mkv")
	loc := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/http/") {
		t.Fatalf("the redirect: %d %q", r.StatusCode, loc)
	}
	sent := ask(t, base, "192.0.2.3", loc)
	if live.StatusCode != 200 || film.StatusCode != 200 || sent.StatusCode != 200 {
		t.Fatalf("before the change: live %d, film %d, film sent on %d", live.StatusCode, film.StatusCode, sent.StatusCode)
	}
	// The VPN goes down: every stream breaks.
	live.Body.Close()
	film.Body.Close()
	sent.Body.Close()
	eventually(t, func() bool { return fp.count(fp.open, "") == 0 && cdn.count(cdn.open, "") == 0 }, "the streams breaking off")
	mu.Lock()
	vpnChanged = mono() // (and comes back up on another address)
	mu.Unlock()

	before := map[string]int{}
	for _, k := range []string{"/live/realU/realP/912.ts", "/movie/realU/realP/911.mkv"} {
		before[k] = fp.count(fp.hits, k)
	}
	beforeCDN := cdn.count(cdn.hits, "/movie/tok/915.mkv")
	for _, c := range []struct{ dev, path, rng string }{
		{"192.0.2.1", "/live/tv/easy/912.ts", ""},
		{"192.0.2.2", "/movie/tv/easy/911.mkv", "bytes=5000000-"},
		{"192.0.2.3", loc, "bytes=5000000-"},
	} {
		var r *http.Response
		if c.rng != "" {
			r = ask(t, base, c.dev, c.path, "Range", c.rng)
		} else {
			r = ask(t, base, c.dev, c.path)
		}
		if r.StatusCode != 503 {
			r.Body.Close()
			t.Errorf("%s asking again for %s right after the address changed: %d, want 503", c.dev, c.path, r.StatusCode)
		} else if text := body(r); r.Header.Get("Retry-After") == "" || !strings.Contains(text, "locks your account") {
			t.Errorf("%s asking again for %s right after the address changed: %q (Retry-After %q)", c.dev, c.path, text, r.Header.Get("Retry-After"))
		}
	}
	for k, n := range before {
		if got := fp.count(fp.hits, k); got != n {
			t.Errorf("the provider was asked for %s from the new address during the wait", k)
		}
	}
	if cdn.count(cdn.hits, "/movie/tok/915.mkv") != beforeCDN {
		t.Errorf("the server the film was sent to was asked from the new address during the wait")
	}

	mu.Lock()
	vpnChanged = mono() - 91 // (the wait is over)
	mu.Unlock()
	again := ask(t, base, "192.0.2.1", "/live/tv/easy/912.ts")
	if again.StatusCode != 200 {
		t.Errorf("the channel after the wait: %d", again.StatusCode)
	}
	again.Body.Close()
}
