package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// The recorder path on an HLS provider (a DVR reads MPEG-TS that Coaxial makes from the channel's HLS), against a
// provider that acts as real ones do: a busy channel stands still for up to 16 s, and big pieces take up to
// 8.7 s to arrive. Neither is a channel that ended or a connection that went quiet.

// recording: a recorder on the channel; how many bytes it has had, and when it last had any (seconds after the
// provider started), and whether its stream ended.
type recording struct {
	got   atomic.Int64
	last  atomic.Int64 // (milliseconds after the provider started)
	ended atomic.Bool
}

func record(t *testing.T, p *realProv) *recording {
	rec := &recording{}
	resp := ask(t, p.base, "192.0.2.9", "/live/tv/easy/44.ts")
	if resp.StatusCode != 200 {
		t.Fatalf("recorder: %d", resp.StatusCode)
	}
	t.Cleanup(func() { resp.Body.Close() })
	rec.got.Store(4096) // (ask reads the first bytes)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				rec.got.Add(int64(n))
				rec.last.Store(time.Since(p.began).Milliseconds())
			}
			if err != nil {
				rec.ended.Store(true)
				return
			}
		}
	}()
	return rec
}

// The channel stands still 16 s at the provider right after the recorder starts (a busy channel at peak time),
// then carries on. The recorder's stream goes on through it and has video again after it: a standstill that
// short is the provider's own pace, not a channel that stopped (the bridge gives up only after 30 s).
func TestRealisticBridgeRidesOutABusyStandstill(t *testing.T) {
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{4}, still: [2]float64{0.01, 12.01}}) // (next move at 16 s)
	rec := record(t, p)
	p.at(18.5)
	if rec.ended.Load() {
		t.Errorf("the recorder's stream ended during a 16 s standstill at the provider")
	}
	if at := float64(rec.last.Load()) / 1000; at < 16 {
		t.Errorf("the recorder had nothing after %.1f s; the channel moved on again at 16 s", at)
	}
	if n := p.opens.Load(); n != 1 {
		t.Errorf("the channel was opened at the provider %d times, want 1", n)
	}
}

// The first piece the recorder's stream needs is 3 MB and arrives over 8.7 s, its server sending all along. It
// is fetched from its one ask and handed on whole, not given up on at pieceWait and asked for again.
func TestRealisticBridgeSlowPieceIsAskedOnce(t *testing.T) {
	const size = 3 << 20 / 188 * 188
	var first atomic.Bool
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{4}, size: size, piece: func(tok, n int, w http.ResponseWriter, r *http.Request) bool {
		if first.CompareAndSwap(false, true) {
			trickle(w, r, tsFill(size/188), 8700*time.Millisecond)
			return true
		}
		return false
	}})
	rec := record(t, p)
	p.at(12)
	if path, n := p.most(); n != 1 {
		t.Errorf("a piece that kept sending for 8.7 s was asked for %d times (%s), want 1", n, path)
	}
	if got := rec.got.Load(); got < size {
		t.Errorf("the recorder had %d bytes after 12 s, want at least the slow piece (%d)", got, size)
	}
}
