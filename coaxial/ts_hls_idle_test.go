package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A device watches a channel as HLS on a provider set to MPEG-TS, and then stops asking: its app was closed,
// its TV switched off, it went out of reach. Nothing tells the proxy; the channel's HLS builder holds the
// provider's connection, and on an account with one connection that is the whole account. By design the
// builder stays for segIdle seconds after the last request, so that a player that only paused carries on
// where it was, and then lets the provider's connection go. A device that comes back later is a new start:
// numbers past the ones it has had, its first piece marked as not following on. Neither half was tested: a
// builder that never left would lock the account until the proxy part is restarted.
func TestLiveHLSFromTSViewerGoesAwayAndComesBack(t *testing.T) {
	p, base := newTSHLSProv(t, 90, 14)
	const dev = "192.0.2.71"
	open := func() (int, int) { p.mu.Lock(); defer p.mu.Unlock(); return p.open["7"], p.asks["7"] }
	status, data, _, at := tsHLSGet(t, dev, base+"/live/tv/easy/7.m3u8")
	last := -1
	read := func(when string) (seq int, text string) {
		t.Helper()
		text = string(data)
		seq, names := tsHLSList(text)
		if status != 200 || seq < 0 || len(names) == 0 || strings.Contains(text, "#EXT-X-ENDLIST") {
			t.Fatalf("%s: playlist %d\n%s", when, status, data)
		}
		return seq, text
	}
	for i := 0; i < 4; i++ { // watching: the playlist once a second
		seq, text := read("watching")
		_, names := tsHLSList(text)
		last = seq + len(names) - 1
		time.Sleep(time.Second)
		status, data, _, _ = tsHLSGet(t, dev, at)
	}
	// A pause shorter than segIdle: the builder and the provider's connection stay, and the playlist carries on.
	time.Sleep(12 * time.Second)
	if n, asks := open(); n != 1 || asks != 1 {
		t.Fatalf("12 s into a pause the provider has %d connections for the channel and was asked %d times; the builder is kept %d s for a player that paused, want 1 and 1", n, asks, int(segIdle))
	}
	status, data, _, _ = tsHLSGet(t, dev, at)
	seq, text := read("after a pause of 12 s")
	_, names := tsHLSList(text)
	if seq > last+1 || seq+len(names)-1 < last {
		t.Errorf("after a pause of 12 s the playlist names pieces %d to %d; the player had pieces up to %d: it does not carry on", seq, seq+len(names)-1, last)
	}
	if _, asks := open(); asks != 1 {
		t.Errorf("a player that carried on after a pause of 12 s had the channel asked for at the provider again (%d asks)", asks)
	}
	last = seq + len(names) - 1
	// Gone for good: the provider's connection is let go.
	left := time.Now()
	freed := time.Duration(0)
	for time.Since(left) < time.Duration(segIdle+12)*time.Second {
		if n, _ := open(); n == 0 {
			freed = time.Since(left)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if freed == 0 {
		t.Fatalf("%d s after the device's last request the channel's connection at the provider is still open: the account's connection is held by a viewer who has gone", int(segIdle+12))
	}
	t.Logf("the provider's connection was let go %.1f s after the device's last request", freed.Seconds())
	// Back again: a new start.
	done := within(t, 5*time.Second, "the playlist of a device that came back")
	began := time.Now()
	status, data, _, _ = tsHLSGet(t, dev, at)
	done()
	seq, text = read("back after the builder left")
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the playlist of a device that came back took %.1f s; the provider's opening burst holds its first two pieces", took.Seconds())
	}
	if seq <= last {
		t.Errorf("back after the builder left, the playlist begins at piece %d; the player has had pieces up to %d and takes a lower number for one it has had", seq, last)
	}
	if disc, first := strings.Index(text, "#EXT-X-DISCONTINUITY\n"), strings.Index(text, "#EXTINF"); disc < 0 || disc > first {
		t.Errorf("back after the builder left, the first piece is not marked as not following on (#EXT-X-DISCONTINUITY before it)\n%s", text)
	}
	if n, asks := open(); n != 1 || asks != 2 {
		t.Errorf("back after the builder left: %d connections at the provider, asked %d times in all; want 1 and 2", n, asks)
	}
	if !bytes.Contains(data, []byte("seg_")) {
		t.Errorf("the playlist names no pieces of the builder's own:\n%s", data)
	}
}
