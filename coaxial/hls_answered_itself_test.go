package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// A provider that answers a live channel's HLS playlist itself, at the channel's own address, instead of sending
// the channel on to another address (both kinds exist: see TestPlaylistKeepsLoginHidden). There is no address to
// remember, so every reload goes to the provider as it would from the player. The channel must still count for
// what a channel sent on counts for: the status list shows the device, and at the connection limit another
// device is turned away instead of let in (the provider would end one of the two streams).

// liveHLS: a provider of live HLS. sentOn false: it answers a channel's playlist itself; true: it sends the
// channel on to another address of its own, which answers the playlist. Its pieces are named on its own server,
// as video. asks: how often a channel's own address was asked.
func liveHLS(t *testing.T, sentOn bool, asks *atomic.Int32) *fakeProv {
	var n atomic.Int32
	playlist := func(w http.ResponseWriter, id int) {
		i := int(n.Add(1))
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:4,\n/hls/realU/realP/%d_%d.ts\n#EXTINF:4,\n/hls/realU/realP/%d_%d.ts\n",
			i, id, i, id, i+1)
	}
	return newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		var id int
		switch {
		case strings.HasPrefix(r.URL.Path, "/hls/realU/realP/"):
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 2000))
		case strings.HasPrefix(r.URL.Path, "/hlsr/"):
			if _, err := fmt.Sscanf(r.URL.Path, "/hlsr/tok/realU/realP/%d/index.m3u8", &id); err != nil {
				return false
			}
			playlist(w, id)
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			if _, err := fmt.Sscanf(r.URL.Path, "/live/realU/realP/%d.m3u8", &id); err != nil {
				return false
			}
			asks.Add(1)
			if sentOn {
				http.Redirect(w, r, fmt.Sprintf("/hlsr/tok/realU/realP/%d/index.m3u8", id), 302)
				return true
			}
			playlist(w, id)
		default:
			return false
		}
		return true
	})
}

// rowsOf: the rows the status list has for device who on this provider.
func rowsOf(f *front, who string) (n int) {
	mu.Lock()
	defer mu.Unlock()
	for _, row := range hlsRows {
		if row.who == who && row.provider == f.base && mono()-row.last <= 30 {
			n++
		}
	}
	return n
}

// channelConns: the connections this provider's account holds for channels now, as the rules count them.
func channelConns(f *front) (n int) {
	mu.Lock()
	defer mu.Unlock()
	for _, c := range f.conns() {
		if c.w.kind == "ch" {
			n++
		}
	}
	return n
}

// playHLS: one look at a channel's playlist by a device, and its newest piece, as a player does.
func playHLS(t *testing.T, base, dev, path string) (int, string) {
	r := ask(t, base, dev, path)
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, dev, loc)
	}
	status, text := r.StatusCode, body(r)
	if status == 200 && strings.Contains(text, "#EXTINF") {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		if p := ask(t, base, dev, lines[len(lines)-1]); p.StatusCode != 200 {
			t.Errorf("%s: a piece of %s: %d", dev, path, p.StatusCode)
		} else {
			body(p)
		}
	}
	return status, text
}

// hlsCounts: the same evening on either kind of provider, with one connection allowed.
func hlsCounts(t *testing.T, sentOn bool) {
	var asks atomic.Int32
	fp := liveHLS(t, sentOn, &asks)
	f, base := limFront(t, fp, "hls", "1", advanced{})
	for i := 0; i < 3; i++ { // the TV tunes and reloads, as a player does
		if status, text := playHLS(t, base, "192.0.2.1", "/live/tv/easy/12.m3u8"); status != 200 || !strings.Contains(text, "#EXTINF") {
			t.Fatalf("the TV's look %d: %d %q", i+1, status, text)
		}
	}
	if n := rowsOf(f, "192.0.2.1"); n != 1 {
		t.Errorf("the TV plays channel 12 as HLS and the status list has %d rows for it, want 1", n)
	}
	// The one connection the account allows is the TV's: another device is turned away, as it is while a channel
	// the provider sent on plays (TestLimitHLSVariantKeepsSession).
	film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
	film.Body.Close()
	if film.StatusCode != 503 {
		t.Errorf("another device asked for a film while the only connection plays live HLS: %d, want 503", film.StatusCode)
	}
	other := ask(t, base, "192.0.2.2", "/live/tv/easy/13.m3u8")
	other.Body.Close()
	if other.StatusCode != 503 {
		t.Errorf("another device asked for another channel while the only connection plays live HLS: %d, want 503", other.StatusCode)
	}
	// Nobody who is on the channel is turned away for it: the TV's own next reload, and a second device that
	// joins the same channel (one channel is one connection, whoever watches it).
	if status, _ := playHLS(t, base, "192.0.2.1", "/live/tv/easy/12.m3u8"); status != 200 {
		t.Errorf("the TV's own reload at the limit: %d, want 200", status)
	}
	if status, _ := playHLS(t, base, "192.0.2.3", "/live/tv/easy/12.m3u8"); status != 200 {
		t.Errorf("a second device joining the same channel at the limit: %d, want 200", status)
	}
	if status, _ := playHLS(t, base, "192.0.2.1", "/live/tv/easy/12.m3u8"); status != 200 {
		t.Errorf("the TV's reload with a second device on the channel: %d, want 200", status)
	}
	// A recorder that asks for the same channel as MPEG-TS is fed from the channel's HLS: it joins, too.
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
	rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Errorf("a recorder joining the channel as MPEG-TS at the limit: %d, want 200", rec.StatusCode)
	}
	if n := channelConns(f); n != 1 {
		t.Errorf("one channel watched by three devices counts as %d connections, want 1", n)
	}
	// What is asked of the provider is what it was: a channel sent on is opened once and its playlist read where
	// it was sent; one the provider answers itself is asked for at its own address every time, by the players
	// (6 looks) and by the recorder's reader.
	mu.Lock()
	kept, own := 0, 0
	for k := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			kept++
			if strings.HasSuffix(k, " live") {
				own++
			}
		}
	}
	mu.Unlock()
	if n := int(asks.Load()); sentOn && (n != 1 || kept != 1 || own != 0) {
		t.Errorf("a channel sent on: its own address was asked %d time(s) and %d session(s) are kept (%d as for a channel answered itself), want 1, 1 and 0", n, kept, own)
	} else if !sentOn && (n < 7 || kept != 1 || own != 1) {
		t.Errorf("a channel answered itself: its own address was asked %d time(s) and %d session(s) are kept (%d of them its own), want 7 or more, 1 and 1", n, kept, own)
	}
	// Once nobody has asked for the channel for a while (hlsIdle), it is the first to make room, as a channel
	// sent on is: the account is not held by a device that has gone.
	mu.Lock()
	for _, ch := range channels { // (the recorder hung up)
		if ch.provider == f.base {
			ch.drop()
		}
	}
	for k, s := range sessions {
		if strings.HasPrefix(k, f.base+" ") {
			s.used -= hlsIdle + 1
		}
	}
	mu.Unlock()
	if status, _ := playHLS(t, base, "192.0.2.2", "/live/tv/easy/13.m3u8"); status != 200 {
		t.Errorf("another device's channel once the TV's had not been asked for in %d s: %d, want 200", int(hlsIdle)+1, status)
	}
}

func TestHLSAnsweredItselfCounts(t *testing.T) { hlsCounts(t, false) }

// The same evening on a provider that sends the channel on (the kind that was counted all along): nothing about
// it changes.
func TestHLSSentOnCountsAsBefore(t *testing.T) { hlsCounts(t, true) }

// The same device changing channel replaces its own connection, as on any other path: it is not refused, and it
// does not wait longer than the moment the provider is given to see the old one go.
func TestHLSAnsweredItselfChannelChange(t *testing.T) {
	var asks atomic.Int32
	fp := liveHLS(t, false, &asks)
	f, base := limFront(t, fp, "hls", "1", advanced{})
	for _, path := range []string{"/live/tv/easy/12.m3u8", "/live/tv/easy/12.m3u8", "/live/tv/easy/13.m3u8", "/live/tv/easy/13.m3u8"} {
		began := mono()
		r := ask(t, base, "192.0.2.1", path)
		if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
			t.Fatalf("%s: %d %q", path, r.StatusCode, text)
		}
		if took := mono() - began; took > 1 {
			t.Errorf("%s took %.1f s, want well under a second", path, took)
		}
	}
	if n := rowsOf(f, "192.0.2.1"); n < 1 {
		t.Errorf("the status list has no row for the TV")
	}
	if n := channelConns(f); n != 1 {
		t.Errorf("after a channel change the TV counts as %d connections, want 1", n)
	}
}

// An answer that is not a playlist (a provider's error page sent with 200, say) plays nothing and counts as nothing.
func TestHLSAnsweredItselfNotAPlaylist(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			return false
		}
		w.Write([]byte("<html>This channel is not available.</html>"))
		return true
	})
	f, base := limFront(t, fp, "hls", "1", advanced{})
	r := ask(t, base, "192.0.2.1", "/live/tv/easy/12.m3u8")
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "not available") {
		t.Fatalf("the provider's own answer was not passed on: %d %q", r.StatusCode, text)
	}
	if rows, conns := rowsOf(f, "192.0.2.1"), channelConns(f); rows != 0 || conns != 0 {
		t.Errorf("an answer that is no playlist left %d row(s) and %d connection(s), want none", rows, conns)
	}
	film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
	film.Body.Close()
	if film.StatusCode != 200 {
		t.Errorf("another device's film after a channel that did not play: %d, want 200", film.StatusCode)
	}
}

// A channel the provider answers itself with a list of quality levels (a master playlist): the player then asks
// for one level's own playlist only. Wherever that playlist is on the provider's server, the TV is never turned
// away or held up for it at the limit; where it is next to the channel's address or below, the channel goes on
// counting while it is asked for.
func TestHLSAnsweredItselfVariants(t *testing.T) {
	for _, level := range []string{"tracks-v1/mono.m3u8", "/hls/realU/realP/45/mono.m3u8"} {
		fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.URL.Path == "/live/realU/realP/45.m3u8":
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\n%s\n", level)
			case strings.HasSuffix(r.URL.Path, "/mono.m3u8"):
				fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n45_1.ts\n")
			default:
				return false
			}
			return true
		})
		f, base := limFront(t, fp, "hls", "1", advanced{})
		r := ask(t, base, "192.0.2.1", "/live/tv/easy/45.m3u8")
		text := body(r)
		lines := strings.Split(strings.TrimSpace(text), "\n")
		if r.StatusCode != 200 || !strings.Contains(text, "#EXT-X-STREAM-INF") {
			t.Fatalf("%s: the master playlist: %d %q", level, r.StatusCode, text)
		}
		for i := 0; i < 3; i++ {
			began := mono()
			v := ask(t, base, "192.0.2.1", lines[len(lines)-1])
			if text := body(v); v.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
				t.Fatalf("%s: look %d at the level's playlist: %d %q", level, i+1, v.StatusCode, text)
			}
			if took := mono() - began; took > 1 {
				t.Errorf("%s: look %d at the level's playlist took %.1f s, want well under a second", level, i+1, took)
			}
		}
		if strings.HasPrefix(level, "/") {
			continue
		}
		if n := channelConns(f); n != 1 {
			t.Errorf("%s: the channel counts as %d connections while its level is asked for, want 1", level, n)
		}
		film := ask(t, base, "192.0.2.2", "/movie/tv/easy/1.mkv")
		film.Body.Close()
		if film.StatusCode != 503 {
			t.Errorf("%s: another device while the only connection plays live HLS: %d, want 503", level, film.StatusCode)
		}
	}
}
