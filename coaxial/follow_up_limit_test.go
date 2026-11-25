package main

import (
	"net/http"
	"strings"
	"testing"
)

// A film the provider sends on to another server, at an account that allows 5 connections. Followed as players
// do, the film counts once and plays, and seeking in it (a Range request to the address it was sent to)
// plays. Then the player lets go of its connection (paused a long time), a recorder and other
// devices take every connection, and the player resumes at the address it was sent to: that is a sixth
// connection, so it is refused like any other new stream rather than letting the provider cut someone.
func TestFollowUpWithinLimit(t *testing.T) {
	cdn := newFakeProv(t, true, nil)
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/movie/realU/realP/905.") {
			http.Redirect(w, r, cdn.srv.URL+"/movie/tok/905.mkv?t=abc", 302)
			return true
		}
		return false
	})
	_, base := limFront(t, fp, "ts", "5", advanced{})
	atProvider := func() int { return fp.count(fp.open, "") + cdn.count(cdn.open, "") }
	var open []*http.Response
	defer func() {
		for _, r := range open {
			r.Body.Close()
		}
	}()
	for i, ch := range []string{"901", "902", "903"} { // three devices watching live channels
		r := ask(t, base, "192.0.2."+string(rune('1'+i)), "/live/tv/easy/"+ch+".ts")
		if r.StatusCode != 200 {
			t.Fatalf("live %s: %d", ch, r.StatusCode)
		}
		open = append(open, r)
	}

	r := ask(t, base, "192.0.2.9", "/movie/tv/easy/905.mkv")
	loc := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/http/") {
		t.Fatalf("the redirect: %d %q", r.StatusCode, loc)
	}
	film := ask(t, base, "192.0.2.9", loc)
	if film.StatusCode != 200 {
		t.Fatalf("the film at the address it was sent to: %d", film.StatusCode)
	}
	film.Body.Close() // the player seeks: it drops its read and asks for the new place
	seek := ask(t, base, "192.0.2.9", loc, "Range", "bytes=500000-")
	if seek.StatusCode != 200 {
		t.Fatalf("seeking in the film: %d", seek.StatusCode)
	}
	eventually(t, func() bool { return atProvider() == 4 }, "four streams at the provider (three channels and the film)")

	// The player lets go of the film; a recorder and another device take every connection meanwhile.
	seek.Body.Close()
	eventually(t, func() bool { return cdn.count(cdn.open, "") == 0 }, "the film's connection closing")
	for _, p := range []struct{ dev, path string }{{"192.0.2.50", "/live/tv/easy/904.ts"}, {"192.0.2.5", "/movie/tv/easy/906.mkv"}} {
		r := ask(t, base, p.dev, p.path)
		if r.StatusCode != 200 {
			t.Fatalf("%s %s with a connection free: %d", p.dev, p.path, r.StatusCode)
		}
		open = append(open, r)
	}
	eventually(t, func() bool { return atProvider() == 5 }, "five streams at the provider")
	resume := ask(t, base, "192.0.2.9", loc, "Range", "bytes=900000-")
	if resume.StatusCode != 503 {
		resume.Body.Close()
		t.Errorf("the film resumed at the address it was sent to with every connection in use: %d, want 503", resume.StatusCode)
	} else if text := body(resume); !strings.Contains(text, "connections your provider allows are in use") {
		t.Errorf("the film resumed with every connection in use: %q", text)
	}
	if n := cdn.count(cdn.hits, "/movie/tok/905.mkv"); n != 2 {
		t.Errorf("the server the film was sent to was asked %d times, want 2 (no third, over the limit)", n)
	}
}
