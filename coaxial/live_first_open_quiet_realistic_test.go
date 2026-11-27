package main

import (
	"context"
	"testing"
	"time"
)

// FAILS ON MAIN (b352eb3): kept apart so the integrator can decide; see REPORT.md.
//
// A channel's FIRST provider connection goes quiet: the provider takes the request and says nothing, or answers
// 200 and then sends no video, without closing (through a VPN tunnel that happens; rule 14). A player straight at
// the provider gives up after its own read timeout (Media3 8 s, AetherEngine 20 s) and asks again on a new
// connection, which plays. Through Coaxial the same wait must end in a new provider connection on the player's
// own request, the way a quiet connection of a channel already playing is asked again (liveQuiet) and the way
// the HLS-to-TS bridge asks a quiet playlist again (pieceWait). Instead the channel is closed and every waiting
// player is answered 504: after liveQuiet (20 s) for a quiet answer, and after connectWait+headerWait (30 s)
// for no answer, with no new connection ever made. A player that retries meanwhile lands on the same waiting
// channel.
func TestLiveTSRealisticFirstOpenQuietAskedAgain(t *testing.T) {
	for _, c := range []struct {
		name string
		conn rpConn
	}{{"answered then quiet", rpConn{noBytes: true}}, {"never answered", rpConn{noHead: true}}} {
		t.Run(c.name, func(t *testing.T) {
			p := newTSRealProv(t, 1, 0)
			feed := rpBuild(50, 1000, 27)
			p.add("7", &rpChannel{feed: feed, lumps: rpLumps(14, 6, 12, 18, 24), back: 10, conns: []rpConn{c.conn}})
			base := rpFront(t, p, "ts", "")
			const end = liveQuiet + 8.0
			ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
			defer cancel()
			rd := rpRead(ctx, p, base, "192.0.2.60", "/live/tv/easy/7.ts", 0, 0)
			rpWaitAll(rd)
			rd.mu.Lock()
			status, n, firstAt := rd.status, len(rd.data), -1.0
			if len(rd.t) > 0 {
				firstAt = rd.t[0]
			}
			rd.mu.Unlock()
			if status != 200 || n == 0 {
				t.Fatalf("the player was answered %d with %d bytes by %.0f s (the provider was asked %d times); "+
					"a player straight at the provider would have asked again on a new connection and played",
					status, n, end, p.asked("7"))
			}
			if firstAt > liveQuiet+6 {
				t.Errorf("first video at %.1f s", firstAt)
			}
		})
	}
}

// The same on the HLS built from the MPEG-TS: the player's first playlist waits on the quiet first connection
// and is answered 504.
func TestHLSFromTSRealisticFirstOpenQuietAskedAgain(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(50, 1000, 27)
	p.add("7", &rpChannel{feed: feed, lumps: rpLumps(14, 6, 12, 18, 24), back: 10, conns: []rpConn{{noBytes: true}}})
	base := rpFront(t, p, "ts", "")
	st, list, _ := rpGet(t, base, "192.0.2.61", "/live/tv/easy/7.m3u8")
	if pieces, _, _, _, _ := rpList(list); st != 200 || len(pieces) == 0 {
		t.Fatalf("the player's first playlist was answered %d (%d pieces) after %.1f s; the provider was asked %d times",
			st, len(pieces), p.wall(), p.asked("7"))
	}
}

// HLS built from the MPEG-TS, with a first connection that answers late (10 s) and then sends nothing: its first
// video is due within firstWait of the ask, not firstWait again after the answer, so the new connection goes out
// in time for the first playlist (segWait, 20 s). With the wait started over at the answer, it went out at 22 s
// and the playlist was answered 504.
func TestHLSFromTSRealisticFirstOpenAnsweredLateThenQuiet(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(50, 1000, 27)
	p.add("7", &rpChannel{feed: feed, lumps: rpLumps(14, 6, 12, 18, 24), back: 10, conns: []rpConn{{headAt: 10, noBytes: true}}})
	base := rpFront(t, p, "ts", "")
	st, list, _ := rpGet(t, base, "192.0.2.62", "/live/tv/easy/7.m3u8")
	if pieces, _, _, _, _ := rpList(list); st != 200 || len(pieces) == 0 {
		t.Fatalf("the player's first playlist was answered %d (%d pieces) after %.1f s; the provider was asked %d times",
			st, len(pieces), p.wall(), p.asked("7"))
	}
}
