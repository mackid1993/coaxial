package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// With "One connection per channel" on and a one-line account: a player paused on a live HLS channel for longer
// than the session is held, then played again, gets the channel back (nothing else is on it). Another device
// tuning the same channel meanwhile watches along on the same session, not refused.
func TestOnePerChannelHLSPausedAndResumed(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" {
			http.Redirect(w, r, "/hlsr/tok44/realU/realP/44/index.m3u8", 302)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:6,\n44_1.ts\n#EXTINF:6,\n44_2.ts\n")
			return true
		}
		return false
	})
	f, base := limFront(t, fp, "hls", "1", advanced{OnePerChannel: true})
	play := func(dev, path string) (int, string) {
		r := ask(t, base, dev, path)
		for r.StatusCode == 302 {
			loc := r.Header.Get("Location")
			r.Body.Close()
			r = ask(t, base, dev, loc)
		}
		return r.StatusCode, body(r)
	}
	if s, text := play("192.0.2.1", "/live/tv/easy/44.m3u8"); s != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("live HLS: %d %q", s, text)
	}
	mu.Lock() // paused: the player stops reloading the playlist for longer than the session is held
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= s.held() + 5
			for d := range s.who {
				s.who[d] -= s.held() + 5
			}
		}
	}
	mu.Unlock()
	before := fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8")
	// Play again: the player reloads the playlist address it was given.
	if s, text := play("192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8"); s != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("the paused player played again: %d %q", s, text)
	}
	if fp.count(fp.hits, "/hlsr/tok44/realU/realP/44/index.m3u8") == before {
		t.Errorf("the channel was not asked of the provider again after the pause")
	}
	if s, text := play("192.0.2.2", "/live/tv/easy/44.m3u8"); s != 200 || !strings.Contains(text, "#EXTINF") {
		t.Errorf("a second device tuning the same channel: %d %q", s, text)
	}
}
