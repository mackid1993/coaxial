package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// A player watching a catch-up as HLS goes back and forth in it. Each time, it asks for the catch-up afresh and
// the provider sends it to a new address, so each time a new playlist is kept for it (keepDirect). A catch-up
// that was read is remembered for hours (catchUpKept), so that a paused player still counts when it carries on.
// The playlists the player has left must not pile up for those hours: a long catch-up's playlist names over a
// thousand pieces. The same device asking for the same channel's catch-up again replaces the one it had.
// Another device's catch-up of that channel is its own and is kept.
func TestCatchUpHLSReaskReplacesItsPlaylist(t *testing.T) {
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
	var asks atomic.Int32
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") { // (every ask is sent to an address of its own)
			http.Redirect(w, r, fmt.Sprintf("%s/arch/tok%d/index.m3u8", cdn.srv.URL, asks.Add(1)), 302)
			return true
		}
		return false
	})
	f, base := limFront(t, fp, "ts", "5", advanced{})
	open := func(dev string) []string {
		t.Helper()
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
				pieces = append(pieces, line[i:])
			}
		}
		if r.StatusCode != 200 || len(pieces) != 30 {
			t.Fatalf("%s: the catch-up's playlist: %d %q", dev, r.StatusCode, text)
		}
		return pieces
	}
	read := func(dev, piece string) int {
		t.Helper()
		p := ask(t, base, dev, piece)
		p.Body.Close()
		return p.StatusCode
	}
	kept := func() (n int) {
		mu.Lock()
		defer mu.Unlock()
		for k := range sessions {
			if strings.HasPrefix(k, f.base+" ") && strings.HasSuffix(k, " direct") {
				n++
			}
		}
		return
	}
	const one, two = "192.0.2.81", "192.0.2.82"
	first := open(one)
	if s := read(one, first[0]); s != 200 {
		t.Fatalf("the first piece: %d", s)
	}
	theirs := open(two)
	if s := read(two, theirs[0]); s != 200 {
		t.Fatalf("the other device's first piece: %d", s)
	}
	if n := kept(); n != 2 {
		t.Fatalf("two devices each watch a catch-up of the channel: %d playlists are kept, want 2", n)
	}
	var newest []string
	for i := 0; i < 40; i++ { // the first device goes back and forth
		newest = open(one)
		if s := read(one, newest[0]); s != 200 {
			t.Fatalf("ask %d: its first piece: %d", i, s)
		}
	}
	if n := kept(); n != 2 {
		t.Errorf("a device asked for the channel's catch-up 41 times in all: %d playlists are kept, want 2 (its newest, and the other device's)", n)
	}
	if s := read(one, newest[1]); s != 200 {
		t.Errorf("a piece of the newest playlist: %d", s)
	}
	mu.Lock()
	row := hlsRows[one+" "+f.base+" 12 catch-up"]
	mu.Unlock()
	if row == nil {
		t.Errorf("the catch-up being read has no row in the status list")
	}
	if s := read(two, theirs[1]); s != 200 {
		t.Errorf("the other device's catch-up, after the first went back and forth: its next piece: %d", s)
	}
	// A piece of a playlist the device has left is still passed on if a request for it is on its way.
	if s := read(one, first[2]); s != 200 {
		t.Errorf("a piece of a playlist the device has left: %d, want it still answered", s)
	}
}
