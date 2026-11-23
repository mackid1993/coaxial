package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Two providers in the settings can be two accounts (lines) on one server address. Each account has its own
// connections: what one account plays is not counted against the other, and is never cut to make room there.
func TestLimitPerAccountOnOneServer(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, baseA := advFront(t, fp, "ts", advanced{Connections: "1"})
	fB := newFront(provider{Name: "Line 2", URL: fp.srv.URL, User: "lineU", Password: "lineP", Port: "18602", LiveTV: "ts",
		Advanced: advanced{Connections: "1"}}, player{User: "tv", Password: "easy"})
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
		fB.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srvB.CloseClientConnections()
		srvB.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == fB.base {
				ch.drop()
			}
		}
		mu.Unlock()
	})
	a := ask(t, baseA, "192.0.2.1", "/live/tv/easy/12.ts")
	if a.StatusCode != 200 {
		t.Fatalf("line 1: %d", a.StatusCode)
	}
	defer a.Body.Close()
	b := ask(t, srvB.URL, "192.0.2.2", "/live/tv/easy/13.ts")
	if b.StatusCode != 200 {
		t.Fatalf("another device on line 2, which has nothing open: %d %q", b.StatusCode, body(b))
	}
	b.Body.Close()
	eventually(t, func() bool { return fp.count(fp.open, "ch13") == 0 }, "line 2's channel closing")
	same := ask(t, srvB.URL, "192.0.2.1", "/live/tv/easy/14.ts") // the device on line 1 also opens line 2
	if same.StatusCode != 200 {
		t.Fatalf("the same device on line 2: %d %q", same.StatusCode, body(same))
	}
	defer same.Body.Close()
	time.Sleep(300 * time.Millisecond)
	if fp.count(fp.open, "ch12") != 1 {
		t.Errorf("line 1's channel was cut to make room on line 2")
	}
}
