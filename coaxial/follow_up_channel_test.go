package main

import (
	"net/http"
	"strings"
	"testing"
)

// Catch-up the provider sends on to another server: the device asks for that address again later (it resumes
// or seeks) while another device holds the channel (a recorder on it live). The provider allows one stream per
// channel, so it is refused like any second stream of the channel; let through, it would end the recording.
func TestOnePerChannelOldFollowUp(t *testing.T) {
	cdn := newFakeProv(t, true, nil)
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") {
			http.Redirect(w, r, cdn.srv.URL+"/timeshift/tok/12.ts?t=abc", 302)
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	r := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts")
	loc := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/http/") {
		t.Fatalf("the redirect: %d %q", r.StatusCode, loc)
	}
	c := ask(t, base, "192.0.2.1", loc)
	if c.StatusCode != 200 {
		t.Fatalf("the catch-up sent on: %d", c.StatusCode)
	}
	c.Body.Close()
	eventually(t, func() bool { return cdn.count(cdn.open, "") == 0 }, "the catch-up closing")
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
	if rec.StatusCode != 200 {
		t.Fatalf("the recorder: %d", rec.StatusCode)
	}
	defer rec.Body.Close()
	again := ask(t, base, "192.0.2.1", loc, "Range", "bytes=1000000-")
	if again.StatusCode != 503 {
		again.Body.Close()
		t.Errorf("the old catch-up address asked again while a recorder is on the channel: %d", again.StatusCode)
	} else if text := body(again); !strings.Contains(text, "one stream per channel") {
		t.Errorf("the old catch-up address asked again while a recorder is on the channel: %q", text)
	}
	if n := cdn.count(cdn.hits, "/timeshift/tok/12.ts"); n != 1 {
		t.Errorf("the catch-up server was asked %d times, want 1", n)
	}
}
