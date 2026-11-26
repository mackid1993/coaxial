package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Two providers in the settings can be two accounts (lines) on one server address. A media server with both
// lines plays channel 7 as HLS on line 1 (the provider is set to MPEG-TS, so the playlist is built here) and
// then starts something on line 2. That frees nothing on line 1 and the device has not left channel 7: its
// builder and its provider connection stay, and the next look at the playlist carries on. (They were let go, as
// for a channel change on one account: channel 7 was opened again at the provider at the next look, and the
// player was handed a playlist that started over.)
func TestLiveHLSFromTSKeptWhenAnotherLineOpens(t *testing.T) {
	for _, second := range []string{"/live/tv/easy/8.ts", "/live/tv/easy/8.m3u8"} {
		p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: 34, clock: 1000}), burst: 14,
			asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
		fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/live/") || !strings.HasSuffix(r.URL.Path, ".ts") {
				return false
			}
			p.serve(w, r)
			return true
		})
		_, baseA := limFront(t, fp, "ts", "1", advanced{})
		fB := newFront(provider{Name: "Line 2", URL: fp.srv.URL, User: "lineU", Password: "lineP", Port: "18602", LiveTV: "ts",
			Connections: "1"}, player{User: "tv", Password: "easy"})
		srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
			fB.ServeHTTP(w, r)
		}))
		t.Cleanup(func() {
			srvB.CloseClientConnections()
			srvB.Close()
			mu.Lock()
			for _, ch := range channels {
				if ch.provider == fB.base {
					ch.drop()
				}
			}
			mu.Unlock()
		})
		status, data, _, at := tsHLSGet(t, "192.0.2.71", baseA+"/live/tv/easy/7.m3u8")
		if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
			t.Fatalf("channel 7 as HLS on line 1: %d %q", status, data)
		}
		if strings.HasSuffix(second, ".ts") { // the same device starts a recording on line 2
			rec := ask(t, srvB.URL, "192.0.2.71", second)
			if rec.StatusCode != 200 {
				t.Fatalf("%s on line 2: %d", second, rec.StatusCode)
			}
			defer rec.Body.Close()
		} else if st, list, _, _ := tsHLSGet(t, "192.0.2.71", srvB.URL+second); st != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
			t.Fatalf("%s on line 2: %d %q", second, st, list)
		}
		time.Sleep(500 * time.Millisecond)
		p.mu.Lock()
		open := p.open["7"]
		p.mu.Unlock()
		if open != 1 {
			t.Errorf("%s was opened on line 2 and channel 7 on line 1 has %d connections at the provider, want 1: it was let go", second, open)
		}
		status, data, _, _ = tsHLSGet(t, "192.0.2.71", at) // the player's next look at channel 7's playlist
		if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
			t.Errorf("after %s was opened on line 2, channel 7's playlist on line 1: %d %q", second, status, data)
		}
		p.mu.Lock()
		asks := p.asks["7"]
		p.mu.Unlock()
		if asks != 1 {
			t.Errorf("after %s was opened on line 2 the provider was asked for channel 7 %d times, want once", second, asks)
		}
		mu.Lock() // (the next round starts with nothing open)
		for _, ch := range channels {
			if ch.provider == fB.base {
				ch.drop()
			}
		}
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
}
