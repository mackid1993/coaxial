package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A provider that sends in lumps every 6 to 12 s and drops a stream now and then, limit 5, one_per_channel on.
// A DVR records channel 200. A TV watches 201, then changes to 202 (fresh opens). A few seconds after
// the TV's change, the provider drops the recorded channel, as it does now and then.
func TestRealRecordingProviderDropAfterTVZap(t *testing.T) {
	var mx sync.Mutex
	asked := map[string]int{}
	drop200 := make(chan struct{})
	lumps := func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/") {
			return false
		}
		mx.Lock()
		asked[r.URL.Path]++
		n := asked[r.URL.Path]
		mx.Unlock()
		ctx := r.Context()
		if r.URL.Path == "/live/realU/realP/200.ts" && n == 1 {
			c, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() { <-drop200; cancel() }()
			ctx = c
		}
		chunk := make([]byte, 188*2000) // a lump
		for i := 0; i < len(chunk); i += 188 {
			chunk[i] = 0x47
		}
		w.Header().Set("Content-Type", "video/mp2t")
		gap := []time.Duration{6 * time.Second, 12 * time.Second, 8 * time.Second}
		for i := 0; ; i++ {
			if _, err := w.Write(chunk); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			select {
			case <-ctx.Done():
				return true
			case <-time.After(gap[i%3] / 6): // (lumps scaled down so the test runs in reopenAfter + a few s)
			}
		}
	}
	fp := newFakeProv(t, false, lumps)
	_, base := limFront(t, fp, "ts", "5", advanced{OnePerChannel: true})
	dvr := drain(ask(t, base, "192.0.2.50", "/live/tv/easy/200.ts"))
	defer dvr.close()
	time.Sleep(2 * time.Second)
	tv := drain(ask(t, base, "192.0.2.20", "/live/tv/easy/201.ts"))
	time.Sleep((reopenAfter - 2) * time.Second)
	tv.close() // the TV changes channel
	tv = drain(ask(t, base, "192.0.2.20", "/live/tv/easy/202.ts"))
	defer tv.close()
	time.Sleep(5 * time.Second)
	close(drop200) // the provider drops the recorded channel 5 s after the TV's change
	time.Sleep(3 * time.Second)
	mx.Lock()
	again := asked["/live/realU/realP/200.ts"]
	mx.Unlock()
	select {
	case <-dvr.done:
		t.Errorf("the recording's connection was closed when the provider dropped its channel (asked %d time(s)); "+
			"the TV had opened another channel 5 s before", again)
	default:
		t.Logf("recording carried on; channel asked %d times", again)
	}
}
