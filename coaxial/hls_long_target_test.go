package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// A live HLS channel with long pieces (a target duration of 15 s or 40 s): the player asks for its playlist
// about once a target duration, so between two asks it is quiet for longer than hlsIdle (or hlsHeld). It is
// still playing: another device at the connection limit is refused, and the player's next ask goes on with
// the same session at the provider. (Also with the playlist a variant on another server.) A player quiet for
// over three target durations has stopped: its connection is free again.
func TestLimitHLSLongPiecesKeepsSession(t *testing.T) {
	variant := regexp.MustCompile(`(?m)^(/\S+mono\.m3u8)$`)
	for _, c := range []struct {
		target, quiet float64 // the playlist's target duration; seconds since the player last asked
		master        bool    // the channel is a list of variants on another server
		stopped       bool    // quiet for longer than three target durations: the player stopped
	}{{15, 14, false, false}, {40, 35, false, false}, {15, 14, true, false}, {15, 46, false, true}, {15, 46, true, true}} {
		name := fmt.Sprintf("target %v, %v s quiet, master %v", c.target, c.quiet, c.master)
		media := fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:%v\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:%v,\n45_1.ts\n", c.target, c.target)
		cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/index.m3u8") {
				io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\ntracks-v1/mono.m3u8\n")
			} else {
				io.WriteString(w, media)
			}
			return true
		})
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == "/live/realU/realP/45.m3u8" && c.master:
				http.Redirect(w, r, cdn.srv.URL+"/edge/45/index.m3u8?t=1", 302)
			case r.URL.Path == "/live/realU/realP/45.m3u8":
				http.Redirect(w, r, "/hlsr/tok45/realU/realP/45/index.m3u8", 302)
			case strings.HasSuffix(r.URL.Path, ".m3u8"):
				io.WriteString(w, media)
			default:
				return false
			}
			return true
		})
		f, base := limFront(t, fp, "hls", "1", advanced{})
		look := func(path string) (int, string) { // the player asks for the playlist
			r := ask(t, base, "192.0.2.1", path)
			for r.StatusCode == 302 {
				loc := r.Header.Get("Location")
				r.Body.Close()
				r = ask(t, base, "192.0.2.1", loc)
			}
			return r.StatusCode, body(r)
		}
		path := "/live/tv/easy/45.m3u8"
		status, text := look(path)
		if c.master {
			m := variant.FindStringSubmatch(text)
			if status != 200 || m == nil {
				t.Fatalf("%s: the master playlist: %d %q", name, status, text)
			}
			path = m[1]
			status, text = look(path)
		}
		if status != 200 || !strings.Contains(text, "#EXTINF") {
			t.Fatalf("%s: the playlist: %d %q", name, status, text)
		}
		mu.Lock() // the player waits a target duration for the next piece
		for k, s := range sessions {
			if strings.HasPrefix(k, f.base+" ") {
				s.used -= c.quiet
				for d := range s.who {
					s.who[d] -= c.quiet
				}
			}
		}
		mu.Unlock()
		film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
		film.Body.Close()
		if c.stopped {
			if film.StatusCode != 200 {
				t.Errorf("%s: another device after the player stopped: %d", name, film.StatusCode)
			}
			continue
		}
		if film.StatusCode != 503 {
			t.Errorf("%s: another device while the only connection plays live HLS: %d", name, film.StatusCode)
		}
		if status, text := look(path); status != 200 || !strings.Contains(text, "#EXTINF") {
			t.Errorf("%s: the player's next ask: %d %q", name, status, text)
		}
		if n := fp.count(fp.hits, "/live/realU/realP/45.m3u8"); n != 1 {
			t.Errorf("%s: the channel was opened at the provider %d times", name, n)
		}
	}
}
