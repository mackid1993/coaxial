package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Live HLS sessions, judged by what a player straight at the provider would do. The fake provider acts like a
// real Xtream one: every ask at a channel's own address opens a NEW session (a new token in the address it
// redirects to) and the newest wins: an older session's playlist and pieces answer 404 from then on.

type newestHLS struct {
	cdn, prov *fakeProv
	base      string
	opens     atomic.Int32 // asks at the channel's own address (each one opens a session at the provider)
	latest    atomic.Int32 // the session that wins
	plAsks    atomic.Int32 // playlist asks, all sessions
	frontHook func(n int, w http.ResponseWriter, r *http.Request) bool
	playHook  func(n int, w http.ResponseWriter, r *http.Request) bool
}

func newNewestHLS(t *testing.T) *newestHLS {
	h := &newestHLS{}
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 2000)
	h.cdn = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		var tok, n int
		if _, err := fmt.Sscanf(r.URL.Path, "/hlsr/tok%d/", &tok); err != nil {
			return false
		}
		if int32(tok) != h.latest.Load() { // newest wins: an older session is gone
			http.NotFound(w, r)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			n = int(h.plAsks.Add(1))
			if h.playHook != nil && h.playHook(n, w, r) {
				return true
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:4,\np%d.ts\n#EXTINF:4,\np%d.ts\n", 100+n, 100+n, 101+n)
			return true
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(piece)
		return true
	})
	h.prov = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/44.m3u8" {
			return false
		}
		n := int(h.opens.Add(1))
		if h.frontHook != nil && h.frontHook(n, w, r) {
			return true
		}
		h.latest.Store(int32(n))
		http.Redirect(w, r, fmt.Sprintf("%s/hlsr/tok%d/index.m3u8", h.cdn.srv.URL, n), 302)
		return true
	})
	_, h.base = limFront(t, h.prov, "hls", "", advanced{})
	return h
}

// devGet: device dev asks for path as a player does (following Coaxial's redirects), giving up after limit.
func devGet(base, dev, path string, limit time.Duration) (int, []byte, error) {
	client := &http.Client{Timeout: limit, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("X-Dev", dev)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode == 302 {
			path = resp.Header.Get("Location")
			resp.Body.Close()
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, data, err
	}
}

// A player that hangs up while its playlist reload is under way (a channel change, an app sent to the
// background, its own read timeout) is not a failed session. Straight at the provider, a hang-up asks nothing
// more of it. Through Coaxial it must not open the channel again at the provider: that new session (newest
// wins) ends the one the other device on the channel is reading, and its next piece is gone.
func TestHLSReloadHangUpDoesNotReopenChannel(t *testing.T) {
	h := newNewestHLS(t)
	var slow atomic.Bool
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if slow.Load() {
			select { // a slow reload (a busy server at peak time): answered after 3 s
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
				return true
			}
		}
		return false
	}
	if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
		t.Fatalf("TV tunes: %d %v", st, err)
	}
	st, list, err := devGet(h.base, "192.0.2.2", "/live/tv/easy/44.m3u8", 10*time.Second)
	if err != nil || st != 200 {
		t.Fatalf("phone tunes: %d %v", st, err)
	}
	slow.Store(true)
	if _, _, err := devGet(h.base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8", time.Second); err == nil {
		t.Fatalf("the TV's reload was meant to be cut off by the TV")
	}
	slow.Store(false)
	time.Sleep(4 * time.Second) // (anything Coaxial does on its own after the hang-up)
	if n := h.opens.Load(); n != 1 {
		t.Errorf("the channel was opened at the provider %d times; the TV only hung up, want 1", n)
	}
	st, data, err := devGet(h.base, "192.0.2.2", pieceOf(t, list), 10*time.Second)
	if err != nil || st != 200 || len(data) == 0 {
		t.Errorf("the phone's next piece, listed a moment ago: status %d, %d bytes, %v (its session was ended)", st, len(data), err)
	}
}

// The provider's own address takes the first open and sends nothing (a connection that went quiet, as through
// a VPN tunnel). A player straight at it gives up after its own read timeout (ExoPlayer 8 s) and asks again on a
// new connection, which answers. Through Coaxial the player's second ask must get through as soon, not wait out
// Coaxial's own 40 s on the first one.
func TestHLSFirstOpenQuietTunesLikeAPlayer(t *testing.T) {
	h := newNewestHLS(t)
	h.frontHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			<-r.Context().Done() // quiet: open, and nothing sent
			return true
		}
		return false
	}
	began := time.Now()
	for time.Since(began) < 70*time.Second {
		st, data, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 8*time.Second) // ExoPlayer's read timeout
		if err == nil && st == 200 && bytes.Contains(data, []byte("#EXTINF")) {
			break
		}
	}
	took := time.Since(began)
	t.Logf("the player had its playlist after %.1f s (front-address asks: %d)", took.Seconds(), h.opens.Load())
	if took > 14*time.Second {
		t.Errorf("tuning took %.1f s; a player straight at the provider has it after its own 8 s timeout and one more ask", took.Seconds())
	}
}

// The provider ends a session and refuses to open the channel again for a while (one provider does so for
// minutes after a re-open, incident 21). A player straight at it retries its own playlist address (ExoPlayer:
// up to 3 more times, 1 s apart and growing; hls.js: no retry of a 4xx); it does not open the channel at the
// provider on every retry. Coaxial's design is "a session that fails is opened again ONCE".
func TestHLSRefusedReopenNotRepeatedEveryReload(t *testing.T) {
	h := newNewestHLS(t)
	var ended atomic.Bool
	h.frontHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			http.Error(w, "Forbidden", 403) // refused for a while after a re-open
			return true
		}
		return false
	}
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if ended.Load() {
			http.Error(w, "Forbidden", 403)
			return true
		}
		return false
	}
	if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
		t.Fatalf("tune: %d %v", st, err)
	}
	ended.Store(true)
	for i, nap := range []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second} { // ExoPlayer's retries
		time.Sleep(nap)
		st, _, _ := devGet(h.base, "192.0.2.1", "/__ch/live/tv/easy/44.m3u8/__index.m3u8", 10*time.Second)
		t.Logf("reload %d: status %d", i+1, st)
	}
	if n := h.opens.Load(); n > 2 {
		t.Errorf("the channel was asked for at the provider's own address %d times in 6 s of refused reloads; the first open and ONE re-open is 2", n)
	}
}

// The recorder path on an HLS provider: a DVR reads MPEG-TS that Coaxial makes from the channel's HLS (the
// bridge). One playlist reload goes quiet on its kept connection. The bridge gives up after pieceWait and asks
// again (as a player would); the channel must not be opened again at the provider for that (incident 21: one
// provider refuses a re-open for minutes), nor may the bridge's own give-up count as the session failing.
func TestHLSBridgeQuietReloadDoesNotReopenChannel(t *testing.T) {
	h := newNewestHLS(t)
	var quiet atomic.Int32 // the playlist ask (by number) that goes quiet
	h.playHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if int32(n) == quiet.Load() {
			<-r.Context().Done()
			return true
		}
		return false
	}
	rec := ask(t, h.base, "192.0.2.9", "/live/tv/easy/44.ts")
	defer rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Fatalf("recorder: %d", rec.StatusCode)
	}
	go io.Copy(io.Discard, rec.Body)
	time.Sleep(2 * time.Second)
	quiet.Store(h.plAsks.Load() + 1) // the next reload goes quiet
	time.Sleep(14 * time.Second)
	if n := h.opens.Load(); n != 1 {
		t.Errorf("the channel was opened at the provider %d times for one quiet reload, want 1", n)
	}
}
