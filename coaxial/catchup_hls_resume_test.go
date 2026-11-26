package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A catch-up watched as HLS is paused for three minutes, and meanwhile another device asks for a playlist of
// its own (any playlist request tidies the sessions up). The catch-up's playlist is complete, so the player does
// not ask for it again when it carries on: it asks for the next piece. That piece must count the catch-up as a
// connection again, so another device asking for the same channel is turned away while it is read. Before, the
// session was forgotten a minute after its last piece, the pieces after the pause belonged to nothing, and
// neither the limit nor one connection per channel saw the catch-up for the rest of the programme.
func TestCatchUpHLSPausedThenResumed(t *testing.T) {
	mu.Lock()
	followUps = map[string]*followUp{}
	for h := range holds {
		delete(holds, h)
	}
	mu.Unlock()
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			text := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n"
			for i := 0; i < 30; i++ {
				text += fmt.Sprintf("#EXTINF:10.0,\npiece%d.ts\n", i)
			}
			w.Write([]byte(text + "#EXT-X-ENDLIST\n"))
			return true
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(make([]byte, 188*500))
		return true
	})
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") {
			http.Redirect(w, r, cdn.srv.URL+"/arch/tok"+channelID(r.URL.Path)+"/index.m3u8", 302)
			return true
		}
		return false
	})
	f, base := limFront(t, fp, "ts", "5", advanced{OnePerChannel: true})
	const dev = "192.0.2.71"
	r := ask(t, base, dev, "/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, dev, loc)
	}
	text := body(r)
	var pieces []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "/__cdn/"); i >= 0 && !strings.HasPrefix(line, "#") {
			pieces = append(pieces, line[i:]) // (named in full, at this proxy's address: the path is asked for)
		}
	}
	if r.StatusCode != 200 || len(pieces) != 30 {
		t.Fatalf("the catch-up's playlist: %d %q", r.StatusCode, text)
	}
	read := func(p string) {
		t.Helper()
		r := ask(t, base, dev, p)
		if n := len(body(r)); r.StatusCode != 200 || n == 0 {
			t.Fatalf("a piece: %d, %d bytes", r.StatusCode, n)
		}
	}
	other := func() int { // another device asks for the same channel, live
		r := ask(t, base, "192.0.2.79", "/live/tv/easy/12.ts")
		r.Body.Close()
		return r.StatusCode
	}
	read(pieces[0])
	read(pieces[1])
	if s := other(); s != 503 {
		t.Fatalf("control: the catch-up is being read and another device's live of the channel got %d, want 503", s)
	}
	mu.Lock() // the player is paused for three minutes
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used, s.opened = s.used-180, s.opened-180
			for d := range s.who {
				s.who[d] -= 180
			}
		}
	}
	mu.Unlock()
	// Meanwhile another device opens a catch-up of another channel as HLS.
	o := ask(t, base, "192.0.2.78", "/timeshift/tv/easy/60/2026-01-01:10-00/13.m3u8")
	o.Body.Close()
	if o.StatusCode != 302 {
		t.Fatalf("another device's catch-up of another channel: %d", o.StatusCode)
	}
	read(pieces[2]) // the player carries on
	if s := other(); s != 503 {
		t.Errorf("a paused catch-up is being read again and another device's live of the channel got %d, want 503", s)
	}
}
