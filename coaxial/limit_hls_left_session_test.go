package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// liveHLSProv: a provider whose live channels are HLS on another server (1 s pieces, the newest three, one more
// each second), each channel sent there with a 302.
func liveHLSProv(t *testing.T) *fakeProv {
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 500)
	start := time.Now()
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/hlsr/") {
			return false
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			n := int(time.Since(start).Seconds())
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", 100+n)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "#EXTINF:1,\np%d.ts\n", 100+n+i)
			}
			return true
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(piece)
		return true
	})
	return newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") || !strings.HasSuffix(r.URL.Path, ".m3u8") {
			return false
		}
		http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok"+channelID(r.URL.Path)+"/index.m3u8", 302)
		return true
	})
}

// A media server is one device that records and serves a viewer, on a provider whose players use HLS (the
// server reads MPEG-TS made from each channel's HLS). The account allows two connections. The viewer watches
// channel 13, a recording of channel 12 starts, and then the viewer changes to channel 14: the server hangs up
// channel 13 and asks for 14. Channel 13's HLS session at the provider is kept for half a minute after its last
// reader, with nobody on it, and still counts. It is what must make room. Instead the newest of the device's own
// connections was cut, which is the recording: it ended the moment the viewer changed channel.
func TestLimitLeftHLSSessionMakesRoomNotRecording(t *testing.T) {
	prov := liveHLSProv(t)
	_, base := limFront(t, prov, "hls", "2", advanced{})
	const server = "192.0.2.5"
	view := ask(t, base, server, "/live/tv/easy/13.ts")
	if view.StatusCode != 200 {
		t.Fatalf("the viewer's channel: %d", view.StatusCode)
	}
	time.Sleep(1200 * time.Millisecond)
	rec := ask(t, base, server, "/live/tv/easy/12.ts") // a recording starts
	defer rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Fatalf("the recording: %d", rec.StatusCode)
	}
	d := drain(rec)
	d.flowing(t, "the recording at its start", 2*time.Second)
	view.Body.Close() // the viewer changes channel: the server hangs up the old one...
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ch := range channels {
			if ch.id == "13" {
				return false
			}
		}
		return true
	}, "channel 13 closing")
	zap := ask(t, base, server, "/live/tv/easy/14.ts") // ...and asks for the new one
	defer zap.Body.Close()
	if zap.StatusCode != 200 {
		t.Fatalf("changing channel at the limit: %d %q", zap.StatusCode, body(zap))
	}
	select {
	case <-d.done:
		t.Fatalf("the recording was cut to make room for the viewer's new channel (the session of the channel the viewer left was there to take)")
	case <-time.After(300 * time.Millisecond):
	}
	d.flowing(t, "the recording, after the viewer changed channel", 3*time.Second)
}
