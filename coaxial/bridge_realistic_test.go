package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// The HLS-to-MPEG-TS bridge (hlsSource) and an HLS player on one channel of a provider set to HLS, through the
// stand-in provider of provider_realistic_test.go. Its playlist states a target duration of 4 s but moves on
// every 6 to 7.5 s, as one provider's does in real use; the channel's address sends the player on to a second
// server; pieces come in uneven writes. A recorder (MPEG-TS made from the HLS) and a viewer on another device
// share the one provider session. Nothing may treat that pace as a stall: the channel is opened at the provider
// once, the recording is every piece in order, whole, with nothing repeated or left out, and the viewer's
// playlist carries on as one.
// Guards the standstill rescue with the bridge on the session (stood: the longer limit when others are on it)
// and the bridge's own end rule (a playlist that stopped moving).
func TestBridgeRealisticOwnPaceRecorderAndViewer(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(100, 1000, 14)
	h := p.addHLS("7", feed, 4, 6, 7.5, 6, 6, 7.5, 6)
	base := rpFront(t, p, "hls", "")
	const end = 27.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	pl := rpPlay(ctx, t, p, base, "192.0.2.51", "/live/tv/easy/7.m3u8", 2)
	time.Sleep(time.Second)
	rec := rpRead(ctx, p, base, "192.0.2.50", "/live/tv/easy/7.ts", 0, 0)
	rpWaitAll(rec)
	time.Sleep(200 * time.Millisecond)
	if n := p.fp.count(p.fp.hits, "/live/realU/realP/7.m3u8"); n != 1 {
		t.Errorf("the channel was opened at the provider %d times, want once (its pace is its own, not a stall)", n)
	}
	if rec.ended > 0 {
		t.Errorf("the recording's answer ended at %.1f s", rec.ended)
	}
	got := rec.bytes()
	first := -1
	for i := 0; i+1 < len(h.cut); i++ {
		if pc := h.piece(i); len(got) >= 4096 && bytes.HasPrefix(pc, got[:4096]) {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("the recording (%d bytes) does not start at a piece", len(got))
	}
	var want []byte
	last := first
	for i := first; len(want) < len(got) && i+1 < len(h.cut); i++ {
		want, last = append(want, h.piece(i)...), i
	}
	if !bytes.Equal(want[:min(len(want), len(got))], got) || len(got) != len(want) {
		at := 0
		for at < len(got) && at < len(want) && got[at] == want[at] {
			at++
		}
		t.Errorf("the recording is not pieces %d to %d whole and in order: it differs at byte %d of %d (%d wanted)", first, last, at, len(got), len(want))
	}
	if newest := h.newest(end); last < newest-1 {
		t.Errorf("the recording reached piece %d; the provider had listed up to %d", last, newest)
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	rpSteady(t, "the viewer", pl)
	if moved := h.newest(end) - 5; len(pl.data) < moved {
		t.Errorf("the viewer got %d pieces; the playlist moved on %d times", len(pl.data), moved)
	}
}
