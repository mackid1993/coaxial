package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// One address reads two channels of a provider set to MPEG-TS at once, at least one as HLS built from the
// MPEG-TS: a TV recording one channel and showing another, a multiview app, a media server. On an account with
// two connections or more, opening the second channel let go of the first channel's HLS builder (a device that
// opens another channel has left its old one), the first channel's next reload made a new builder, which let
// go of the second, and so on: every reload opened a channel at the provider again. Each channel must be asked
// for once.
func TestOneAddressTwoChannelsAsHLSFromTSKeepsBoth(t *testing.T) {
	for _, second := range []string{"m3u8", "ts"} {
		t.Run("second as "+second, func(t *testing.T) {
			p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: 34, clock: 1000}), burst: 14,
				asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
			fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
				if len(r.URL.Path) < 18 || r.URL.Path[:18] != "/live/realU/realP/" {
					return false
				}
				p.serve(w, r)
				return true
			})
			_, base := limFront(t, fp, "ts", "2", advanced{})
			const dev = "192.0.2.72"
			if st, _, _, _ := tsHLSGet(t, dev, base+"/live/tv/easy/7.m3u8"); st != 200 {
				t.Fatalf("channel 7 as HLS: %d", st)
			}
			stop := func() {}
			if second == "ts" { // channel 8 read as live MPEG-TS for the whole test
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					req, _ := http.NewRequestWithContext(ctx, "GET", base+"/live/tv/easy/8.ts", nil)
					req.Header.Set("X-Dev", dev)
					if resp, err := devClient.Do(req); err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}()
				stop = func() { cancel(); <-done }
				time.Sleep(500 * time.Millisecond)
			}
			defer func() { stop() }()
			for round := 0; round < 4; round++ { // both reload about once a second
				if st, _, _, _ := tsHLSGet(t, dev, base+"/live/tv/easy/7.m3u8"); st != 200 {
					t.Fatalf("round %d, channel 7: %d", round, st)
				}
				if second == "m3u8" {
					if st, _, _, _ := tsHLSGet(t, dev, base+"/live/tv/easy/8.m3u8"); st != 200 {
						t.Fatalf("round %d, channel 8: %d", round, st)
					}
				}
				time.Sleep(time.Second)
			}
			p.mu.Lock()
			a7, a8 := p.asks["7"], p.asks["8"]
			p.mu.Unlock()
			if a7 != 1 || a8 != 1 {
				t.Errorf("one address on two channels: the provider was asked for channel 7 %d times and channel 8 %d times, want once each", a7, a8)
			}
		})
	}
}
