package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A provider connection can go quiet without closing (through a VPN tunnel that happens). A player asking the
// server itself gives up on it and reconnects; through Coaxial it must not wait longer than a player would.

// quietHLS: a provider that sends its live HLS channel to a second server (as real ones send it to a CDN), whose
// playlist and pieces behave as told by quiet (the n-th ask of a path, from 1: what to do).
func quietHLS(t *testing.T, quiet func(path string, n int, w http.ResponseWriter, r *http.Request) bool) (cdn, prov *fakeProv, base string) {
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1<<20/188)
	asked := map[string]*atomic.Int32{}
	for _, p := range []string{"/hlsr/tok44/index.m3u8", "/hlsr/tok44/p1.ts"} {
		asked[p] = &atomic.Int32{}
	}
	cdn = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		c := asked[r.URL.Path]
		if c == nil {
			return false
		}
		if quiet(r.URL.Path, int(c.Add(1)), w, r) {
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\np1.ts\n")
			return true
		}
		w.Header().Set("Content-Type", "video/mp2t")
		http.ServeContent(w, r, "p1.ts", time.Time{}, bytes.NewReader(piece)) // (Range answered as servers do)
		return true
	})
	prov = newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" {
			http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok44/index.m3u8", 302)
			return true
		}
		return false
	})
	_, base = limFront(t, prov, "hls", "", advanced{})
	return cdn, prov, base
}

// playerGet: a player's request, following Coaxial's own redirects; the answer's status and whole body.
func playerGet(t *testing.T, base, path string) (int, []byte, time.Duration) {
	t.Helper()
	began := time.Now()
	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("X-Dev", "192.0.2.1")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, time.Since(began)
		}
		if resp.StatusCode == 302 {
			path = resp.Header.Get("Location")
			resp.Body.Close()
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return -1, data, time.Since(began)
		}
		return resp.StatusCode, data, time.Since(began)
	}
}

func pieceOf(t *testing.T, list []byte) string {
	for _, l := range strings.Split(string(list), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	t.Fatalf("no piece in the playlist %q", list)
	return ""
}

// The piece's server takes the request and says nothing: asked again on a new connection after pieceWait, the
// player gets the whole piece then, not after the full 60 s.
func TestHLSPieceQuietServerAskedAgain(t *testing.T) {
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(path, ".ts") && n == 1 {
			<-r.Context().Done()
			return true
		}
		return false
	})
	status, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	if status != 200 {
		t.Fatalf("playlist: %d", status)
	}
	done := within(t, secs(pieceWait+4), "the piece")
	status, data, took := playerGet(t, base, pieceOf(t, list))
	done()
	if status != 200 || len(data) != 1<<20/188*188 {
		t.Fatalf("piece: status %d, %d bytes", status, len(data))
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece took %.1f s; a quiet server must be asked again after %d s", took.Seconds(), pieceWait)
	}
}

// The piece stops part way: the rest is fetched from where it stopped and the player gets one whole piece.
func TestHLSPieceStoppedPartWayResumed(t *testing.T) {
	var ranged atomic.Value
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") {
			return false
		}
		if n == 1 {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Length", fmt.Sprint(1<<20/188*188))
			w.Write(bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1500))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return true
		}
		ranged.Store(r.Header.Get("Range"))
		return false
	})
	_, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	done := within(t, secs(pieceWait+4), "the piece")
	status, data, took := playerGet(t, base, pieceOf(t, list))
	done()
	if status != 200 || len(data) != 1<<20/188*188 {
		t.Fatalf("piece: status %d, %d bytes, want the whole piece on one answer", status, len(data))
	}
	if g, _ := ranged.Load().(string); g != fmt.Sprintf("bytes=%d-", 1500*188) {
		t.Errorf("the rest was asked for with Range %q", g)
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece took %.1f s", took.Seconds())
	}
}

// A playlist reload that goes quiet is asked again on a new connection, on the same session: the player gets
// its playlist within pieceWait and the channel is not opened again at the provider (some refuse that).
func TestHLSPlaylistQuietReloadSameSession(t *testing.T) {
	_, prov, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(path, ".m3u8") && n == 2 {
			<-r.Context().Done()
			return true
		}
		return false
	})
	if status, _, _ := playerGet(t, base, "/live/tv/easy/44.m3u8"); status != 200 {
		t.Fatalf("first playlist: %d", status)
	}
	done := within(t, secs(pieceWait+4), "the playlist reload")
	status, list, took := playerGet(t, base, "/__ch/live/tv/easy/44.m3u8/__index.m3u8")
	done()
	if status != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
		t.Fatalf("reload: %d %q", status, list)
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the reload took %.1f s", took.Seconds())
	}
	if n := prov.count(prov.hits, "/live/realU/realP/44.m3u8"); n != 1 {
		t.Errorf("the channel was opened at the provider %d times, want 1", n)
	}
}

// quietTS: a live MPEG-TS provider (live_lead_test's feed and timing). Its first connection goes quiet, without
// closing, from the lump at quietAt on, or (quietAt 0) pauses `pause` extra seconds before the lump at pauseAt;
// any later connection is the usual one asked again.
func quietTS(t *testing.T, pt string, quietAt, pauseAt, pause float64) (srv *httptest.Server, asked *atomic.Int32, start time.Time) {
	fd := syntheticFeed()
	start = time.Now()
	asked = &atomic.Int32{}
	again := &atomic.Int32{}
	again.Store(1) // (leadProvider's own count: past 1, it answers as a channel asked for again)
	later := leadProvider(fd, start, 1e9, again)
	lw := lateFor(t)
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1) > 1 {
			later(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		stopAt, extra, from := len(fd.at), 0.0, 0
		if quietAt > 0 {
			stopAt = fd.lumpStart(quietAt)
		}
		for k := 0; k < len(fd.at); k++ {
			if pause > 0 && k == fd.lumpStart(pauseAt) {
				extra = pause
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Until(start.Add(time.Duration((fd.at[k] + extra) * float64(time.Second))))):
			}
			if k == stopAt {
				<-r.Context().Done() // quiet: the connection stays open and sends nothing
				return
			}
			if _, err := w.Write(fd.data[from:fd.end[k]]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			from = fd.end[k]
			lw.sent(from)
		}
		<-r.Context().Done()
	}))
	f := newFront(provider{Name: "Quiet", URL: prov.URL, User: "realU", Password: "realP", Port: port(pt), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	srv = httptest.NewServer(f)
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
	whereOnFail(t)
	return
}

// The provider's connection goes quiet 8 s into a channel: it is asked for again after liveQuiet (not 60 s), the
// players keep their connection and get video again, the clock never runs backwards. (Early on purpose: a
// stream that ran under reopenAfter must still be asked for again when it went quiet.)
func TestLiveTSQuietConnectionAskedAgain(t *testing.T) {
	const quietAt, end = 8.0, 8.0 + liveQuiet + 14
	srv, asked, start := quietTS(t, "18631", quietAt, 0, 0)
	lateFor(t).newBytesBy(start.Add(secs(quietAt+2)), start.Add(secs(quietAt+liveQuiet+6)), "video after the connection went quiet")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(end*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	if n := asked.Load(); n != 2 {
		t.Errorf("the provider was asked %d times, want 2", n)
	}
	resumed := 0.0
	for i, at := range v.t {
		if at > quietAt+2 && i > 0 && v.have[i] > v.have[i-1] {
			resumed = at
			break
		}
	}
	if resumed == 0 || resumed > quietAt+liveQuiet+6 {
		t.Errorf("video came again at %.1f s; the connection went quiet at %.0f s", resumed, quietAt)
	}
	if v.back > 0 {
		t.Errorf("the clock went backwards %d times", v.back)
	}
}

// A long pause on a healthy stream (17 s, as providers make at peak load) is the provider's own pace: nothing
// is asked again and the stream goes on as it came.
func TestLiveTSLongPauseLeftAlone(t *testing.T) {
	const pauseAt, pause = 20.0, 11.0 // (the lumps 6 s apart there become 17 s apart)
	srv, asked, start := quietTS(t, "18632", 0, pauseAt, pause)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration((pauseAt+pause+12)*float64(time.Second)))
	defer cancel()
	v := &leadView{}
	watch(ctx, t, srv.URL+"/live/tv/easy/7.ts", start, v)
	if n := asked.Load(); n != 1 {
		t.Errorf("the provider was asked %d times during a 17 s pause, want 1", n)
	}
	if n := len(v.t); n == 0 || v.t[n-1] < pauseAt+pause {
		t.Errorf("no video after the pause")
	}
}
