package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A recorder is on channel 12. Another device opens channel 13 a few seconds before the provider ends channel 12
// (which ran over reopenAfter). Whether channel 12 is asked for again depends on whose account channel 13 was
// opened on: on the recorder's own one-connection account asking again would end channel 13, so it is not asked
// (the rule of the first version); on another account it ends nothing, so it is.

// reaskCase: one such sequence against a stand-in provider of its own. connections: what is typed in for the
// recorder's account. other: the second provider in the settings the TV asks through (nil: the recorder's own).
// tvStatus: the answer the TV must get (0: the TV plays its channel as HLS, which is built from the channel's
// MPEG-TS, and whatever it is answered counts). asks: how often channel 12 must have been asked for in the end.
func reaskCase(t *testing.T, connections number, other *provider, tvStatus, asks int) {
	var asked atomic.Int32
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/12.ts" || asked.Add(1) != 1 {
			return false
		}
		// (the first ask for the recorder's channel: the provider ends it after reopenAfter seconds and a little)
		w.Header().Set("Content-Type", "video/mp2t")
		chunk := make([]byte, 188*100)
		for i := 0; i < len(chunk); i += 188 {
			chunk[i] = 0x47
		}
		for end := time.Now().Add(secs(reopenAfter + 2)); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if _, err := w.Write(chunk); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
		}
		return true
	})
	_, base := limFront(t, fp, "ts", connections, advanced{})
	tvBase := base
	if other != nil {
		other.URL = fp.srv.URL
		fB := newFront(*other, player{User: "tv", Password: "easy"})
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
		tvBase = srvB.URL
	}
	began := time.Now()
	rec := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts") // the recorder
	if rec.StatusCode != 200 {
		t.Fatalf("the recorder: %d", rec.StatusCode)
	}
	defer rec.Body.Close()
	var got atomic.Int64
	var ended atomic.Bool
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := rec.Body.Read(buf)
			got.Add(int64(n))
			if err != nil {
				ended.Store(true)
				return
			}
		}
	}()
	time.Sleep(time.Until(began.Add(secs(reopenAfter - 5)))) // the TV opens channel 13, 7 s before the drop
	if tvStatus == 0 {
		go func() {
			req, _ := http.NewRequest("GET", tvBase+"/__ch/live/tv/easy/13.m3u8/__index.m3u8", nil)
			req.Header.Set("X-Dev", "192.0.2.2")
			if r, err := devClient.Do(req); err == nil {
				body(r)
			}
		}()
	} else {
		tv := ask(t, tvBase, "192.0.2.2", "/live/tv/easy/13.ts")
		if tv.StatusCode != tvStatus {
			t.Fatalf("the TV's channel: %d %q, want %d", tv.StatusCode, body(tv), tvStatus)
		}
		defer tv.Body.Close()
		go io.Copy(io.Discard, tv.Body)
	}
	time.Sleep(time.Until(began.Add(secs(reopenAfter + 4)))) // 2 s after the provider ended the recorder's channel
	before := got.Load()
	time.Sleep(time.Second)
	n, carries := int(asked.Load()), !ended.Load() && got.Load() > before
	if n != asks {
		t.Errorf("the recorder's channel was asked for %d time(s), want %d", n, asks)
	}
	if asks > 1 && !carries {
		t.Errorf("the recorder gets no more video (its stream ended: %v): the channel the provider ended was not asked for again", ended.Load())
	}
	if asks == 1 && carries {
		t.Errorf("the recorder's channel was asked for again: on a one-connection account that ends the channel just opened")
	}
}

func TestReaskAfterAnotherChannelOpened(t *testing.T) {
	// Two providers in the settings can be two accounts (lines) on one server address. A channel opened on one
	// line ends nothing on the other: a recorder on line 1 lost its recording because a TV on line 2 had changed
	// channel a few seconds before the provider dropped the recorder's channel.
	t.Run("on another account of the same server: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "1", &provider{Name: "Line 2", User: "lineU", Password: "lineP", Port: "18602", LiveTV: "ts", Connections: "1"}, 200, 2)
	})
	t.Run("on another account, opened as HLS: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "1", &provider{Name: "Line 2", User: "lineU", Password: "lineP", Port: "18602", LiveTV: "ts", Connections: "1"}, 0, 2)
	})
	// One account, as nearly every setup has: nothing changes. With no number typed in, the account is taken to
	// allow one connection: the channel just opened ended the recorder's, and asking again would end that one.
	t.Run("on the same account, no limit typed in: not asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "", nil, 200, 1)
	})
	t.Run("on the same account, opened as HLS: not asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "", nil, 0, 1)
	})
	// One account with its one connection typed in: the TV is turned away, so its channel was never opened and
	// the recorder's channel is asked for again.
	t.Run("on the same account, the TV was turned away: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "1", nil, 503, 2)
	})
	// The same login written in twice (a second port with other settings) is still one account at the provider.
	t.Run("the same login on a second port: not asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "1", &provider{Name: "Again", User: "realU", Password: "realP", Port: "18602", LiveTV: "ts", Connections: "1"}, 200, 1)
	})
}

// The same login written in twice is one account, and a channel is one connection on it whichever port its
// readers came in by: the second device joins the channel, and the provider is not asked for the same stream a
// second time (it would end the first).
func TestSameLoginOnTwoPortsIsOneChannel(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, baseA := limFront(t, fp, "ts", "1", advanced{})
	fB := newFront(provider{Name: "Again", URL: fp.srv.URL, User: "realU", Password: "realP", Port: "18602", LiveTV: "ts",
		Connections: "1"}, player{User: "tv", Password: "easy"})
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
		fB.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srvB.CloseClientConnections(); srvB.Close() })
	a := ask(t, baseA, "192.0.2.1", "/live/tv/easy/12.ts")
	if a.StatusCode != 200 {
		t.Fatalf("the first device: %d", a.StatusCode)
	}
	defer a.Body.Close()
	b := ask(t, srvB.URL, "192.0.2.2", "/live/tv/easy/12.ts")
	if b.StatusCode != 200 {
		t.Fatalf("the second device, by the other port: %d %q", b.StatusCode, body(b))
	}
	defer b.Body.Close()
	if asked, most := fp.count(fp.hits, "/live/realU/realP/12.ts"), fp.count(fp.most, "ch12"); asked != 1 || most != 1 {
		t.Errorf("the provider was asked for the channel %d time(s) and had %d connection(s) for it at once, want 1 and 1", asked, most)
	}
}
