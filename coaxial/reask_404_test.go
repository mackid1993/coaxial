package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// reaskFront: a front door on a provider that streams (live_lead_test's provider: opening burst, lumps), drops the
// channel at dropAt, and then answers the next `fails` asks with `status` before it plays again.
func reaskFront(t *testing.T, pt string, fails int32, status int) (srv *httptest.Server, asked *atomic.Int32, start time.Time, done func()) {
	fd := syntheticFeed()
	start = time.Now()
	asked = &atomic.Int32{}
	var served atomic.Int32
	lead := leadProvider(fd, start, reopenAfter+3.0, &served)
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := asked.Add(1)
		if n >= 2 && n <= 1+fails {
			http.Error(w, "stream unavailable", status)
			return
		}
		lead(w, r)
	}))
	f := newFront(provider{Name: "Reask", URL: prov.URL, User: "realU", Password: "realP", Port: port(pt), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv = httptest.NewServer(f)
	done = func() {
		srv.CloseClientConnections()
		srv.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		mu.Unlock()
		prov.CloseClientConnections()
		prov.Close()
	}
	return
}

// After a provider ends a watched channel and answers 404 to the next two asks (its side restarting the channel),
// Coaxial keeps asking and the players get video again, with the clock never running backwards.
func TestReaskThrough404(t *testing.T) {
	old := reaskBackoff
	reaskBackoff = []float64{1, 1, 1, 1}
	defer func() { reaskBackoff = old }()
	srv, asked, start, done := reaskFront(t, "18621", 2, 404)
	defer done()
	const end = reopenAfter + 30.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(end*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	if n := asked.Load(); n != 4 {
		t.Errorf("the provider was asked %d times, want 4 (once, twice answered 404, then once more)", n)
	}
	if v.back > 0 {
		t.Errorf("the stream's clock went backwards %d times", v.back)
	}
	if n := len(v.t); n == 0 || v.t[n-1] < end-15 {
		t.Errorf("no video in the last 15 s: the player was let go after the 404s")
	}
}

// A server error is treated the same way.
func TestReaskThrough503(t *testing.T) {
	old := reaskBackoff
	reaskBackoff = []float64{1, 1, 1, 1}
	defer func() { reaskBackoff = old }()
	srv, asked, start, done := reaskFront(t, "18622", 1, 503)
	defer done()
	const end = reopenAfter + 25.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(end*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	if n := asked.Load(); n != 3 {
		t.Errorf("the provider was asked %d times, want 3", n)
	}
	if n := len(v.t); n == 0 || v.t[n-1] < end-12 {
		t.Errorf("no video at the end: the player was let go after the 503")
	}
}

// A channel that stays gone is given up on after the last extra ask: the player is let go, and the provider is not
// asked again after that.
func TestReaskGivesUp(t *testing.T) {
	old := reaskBackoff
	reaskBackoff = []float64{0.5, 0.5, 0.5}
	defer func() { reaskBackoff = old }()
	srv, asked, start, done := reaskFront(t, "18623", 1000, 404)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration((reopenAfter+30.0)*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	ended := time.Since(start).Seconds()
	if ctx.Err() != nil {
		t.Fatalf("the player was never let go")
	}
	if ended > reopenAfter+3.0+10 {
		t.Errorf("the player was let go %.1f s after the start, want soon after the drop at %.1f s", ended, reopenAfter+3.0)
	}
	time.Sleep(3 * time.Second)
	if n := asked.Load(); n != 1+1+3 {
		t.Errorf("the provider was asked %d times, want 5 (once, once again, three extra asks)", n)
	}
}

// Not every answer is retried: a refusal that means the login or the channel is wrong (here 403) ends it at once.
func TestReaskNot403(t *testing.T) {
	old := reaskBackoff
	reaskBackoff = []float64{0.5, 0.5, 0.5}
	defer func() { reaskBackoff = old }()
	srv, asked, start, done := reaskFront(t, "18624", 1000, 403)
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration((reopenAfter+30.0)*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	time.Sleep(2 * time.Second)
	if n := asked.Load(); n != 2 {
		t.Errorf("the provider was asked %d times after a 403, want 2 (no extra asks)", n)
	}
}
