package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A watched channel's provider connection goes quiet (as through a VPN tunnel). Coaxial asks for it again; the
// new connection answers 200 but is quiet too before its first byte; the connection after that plays. A player
// straight at the provider reconnects again after its own read timeout (Media3 DefaultHttpDataSource 8 s,
// retried by DefaultLoadErrorHandlingPolicy; AetherEngine 20 s) and plays on. Through Coaxial the readers must
// keep their connection and get video again, not be let go with a clean end of stream.
func TestLiveTSReaskQuietBeforeFirstByteAskedAgain(t *testing.T) {
	const quietAt = 8.0
	fd := syntheticFeed()
	start := time.Now()
	var asked, lp atomic.Int32
	lp.Store(1) // (leadProvider's count: past 1 it answers as a channel asked for again)
	later := leadProvider(fd, start, 1e9, &lp)
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch asked.Add(1) {
		case 1: // plays, then goes quiet without closing from the lump at quietAt on
			w.Header().Set("Content-Type", "video/mp2t")
			stopAt, from := fd.lumpStart(quietAt), 0
			for k := 0; k < stopAt; k++ {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Until(start.Add(time.Duration(fd.at[k] * float64(time.Second))))):
				}
				if _, err := w.Write(fd.data[from:fd.end[k]]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				from = fd.end[k]
			}
			<-r.Context().Done()
		case 2: // answers, then quiet before any video
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			later(w, r)
		}
	}))
	f := newFront(provider{Name: "Quiet2", URL: prov.URL, User: "realU", Password: "realP", Port: "18641", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		mu.Unlock()
		prov.CloseClientConnections()
		prov.Close()
	})
	// quiet at 8 s, asked again at 8+liveQuiet, that one quiet for 2*reopenWait, then asked once more
	end := quietAt + liveQuiet + 2*reopenWait + 15
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(end*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	gone := time.Since(start).Seconds()
	if gone < end-1 {
		t.Errorf("the reader's connection was ended at %.1f s (the channel was given up); a player straight at the provider would have reconnected", gone)
	}
	if n := asked.Load(); n < 3 {
		t.Errorf("the provider was asked %d times, want at least 3", n)
	}
	resumed := 0.0
	for i, at := range v.t {
		if at > quietAt+liveQuiet+2*reopenWait && i > 0 && v.have[i] > v.have[i-1] {
			resumed = at
			break
		}
	}
	if resumed == 0 {
		t.Errorf("no video after the second quiet connection (last video at %.1f s)", lastAt(v))
	}
	if v.back > 0 {
		t.Errorf("the clock went backwards %d times", v.back)
	}
}

func lastAt(v *leadView) float64 {
	if len(v.t) == 0 {
		return 0
	}
	return v.t[len(v.t)-1]
}
