package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A live HLS session nobody asked for in over 30 s no longer counts, so catch-up of its channel is let
// through. A device asking for the channel live again then opens it at the provider anew: by the rules, like
// any new session, and is refused while the catch-up plays.
func TestOnePerChannelHLSSessionAgain(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" {
			http.Redirect(w, r, "/hlsr/tok44/realU/realP/44/index.m3u8", 302)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n44_1.ts\n")
			return true
		}
		return false
	})
	f, base := advFront(t, fp, "hls", advanced{StayUnder: off(), OnePerChannel: true})
	open := func(dev string) int {
		r := ask(t, base, dev, "/live/tv/easy/44.m3u8")
		for r.StatusCode == 302 {
			loc := r.Header.Get("Location")
			r.Body.Close()
			r = ask(t, base, dev, loc)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if s := open("192.0.2.1"); s != 200 {
		t.Fatalf("live HLS: %d", s)
	}
	mu.Lock() // the device stopped 35 s ago; the session is still remembered
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= 35
			for d := range s.who {
				s.who[d] -= 35
			}
		}
	}
	mu.Unlock()
	c := ask(t, base, "192.0.2.2", "/timeshift/tv/easy/60/2026-01-01:10-00/44.ts")
	if c.StatusCode != 200 {
		t.Fatalf("catch-up of a channel nobody is on: %d", c.StatusCode)
	}
	defer c.Body.Close()
	before := fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8")
	if s := open("192.0.2.3"); s != 503 {
		t.Errorf("live HLS of a channel whose catch-up plays on another device: %d", s)
	}
	if n := fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8"); n != before {
		t.Errorf("the provider was asked for the channel live while its catch-up plays (%d times)", n-before)
	}
}
