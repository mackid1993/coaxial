package main

import (
	"net/http"
	"strings"
	"testing"
)

// Live MPEG-TS, on the two kinds of provider there are: one that answers a channel's address with the video
// itself, and one that sends the channel on to a video server (a 302), as many do. Either way the channel is one
// shared connection that the rules count and the status list shows: a second device on it joins without the
// provider being asked again, and at the connection limit another channel is turned away.
func TestLiveTSCountsAnsweredItselfOrSentOn(t *testing.T) {
	for _, sentOn := range []bool{false, true} {
		kind := map[bool]string{false: "answered itself", true: "sent on"}[sentOn]
		video := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/play/tok/") {
				return false
			}
			fp.stream(w, r, "ch"+channelID(r.URL.Path), true, 0)
			return true
		})
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			if !sentOn || !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") {
				return false
			}
			http.Redirect(w, r, video.srv.URL+"/play/tok/"+channelID(r.URL.Path)+".ts", 302)
			return true
		})
		f, base := limFront(t, fp, "ts", "1", advanced{})
		tv := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
		if tv.StatusCode != 200 {
			t.Fatalf("%s: the TV's channel: %d %q", kind, tv.StatusCode, body(tv))
		}
		defer tv.Body.Close()
		second := ask(t, base, "192.0.2.2", "/live/tv/easy/12.ts")
		if second.StatusCode != 200 {
			t.Fatalf("%s: a second device on the same channel: %d %q", kind, second.StatusCode, body(second))
		}
		defer second.Body.Close()
		if n := fp.count(fp.hits, "/live/realU/realP/12.ts"); n != 1 {
			t.Errorf("%s: the provider was asked for the channel %d times with two devices on it, want once", kind, n)
		}
		mu.Lock()
		conns, rows := 0, 0
		for _, c := range f.conns() {
			if c.w.kind == "ch" {
				conns++
			}
		}
		for _, ch := range channels {
			if ch.provider == f.base {
				rows += len(ch.readers)
			}
		}
		mu.Unlock()
		if conns != 1 || rows != 2 {
			t.Errorf("%s: one channel with two devices counts as %d connection(s) and has %d row(s) in the status list, want 1 and 2", kind, conns, rows)
		}
		other := ask(t, base, "192.0.2.3", "/live/tv/easy/13.ts")
		other.Body.Close()
		if other.StatusCode != 503 {
			t.Errorf("%s: another device's other channel while the only connection is in use: %d, want 503", kind, other.StatusCode)
		}
		tv.Body.Close()
		second.Body.Close()
		srv := map[bool]*fakeProv{false: fp, true: video}[sentOn]
		eventually(t, func() bool { return srv.count(srv.open, "ch12") == 0 }, kind+": the channel closing at the provider once both devices left")
	}
}
