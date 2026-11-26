package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// On a provider whose players use HLS, a recorder records channel 12 as MPEG-TS (made from the channel's HLS),
// and another device watches the same channel's catch-up as HLS (the programme being recorded, from its start).
// The account allows two connections, and both are in use by those two devices. A third device asking for
// another channel must be turned away, and the recording must go on. The catch-up's own session was taken for
// the live channel's when the connections were counted (both are "channel 12"), so the recorder was counted on
// the catch-up, the live session looked like nobody's, and it was cut to make room: the recording ended.
func TestCatchUpOfRecordedChannelLeavesRecordingAlone(t *testing.T) {
	mu.Lock()
	followUps = map[string]*followUp{}
	for h := range holds {
		delete(holds, h)
	}
	mu.Unlock()
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 500)
	start := time.Now()
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hlsr/") && strings.HasSuffix(r.URL.Path, ".m3u8"):
			// a live playlist: 1 s pieces, the newest three, one more each second
			n := int(time.Since(start).Seconds())
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", 100+n)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "#EXTINF:1,\np%d.ts\n", 100+n+i)
			}
		case strings.HasPrefix(r.URL.Path, "/arch/") && strings.HasSuffix(r.URL.Path, ".m3u8"):
			text := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n"
			for i := 0; i < 30; i++ {
				text += fmt.Sprintf("#EXTINF:10.0,\npiece%d.ts\n", i)
			}
			w.Write([]byte(text + "#EXT-X-ENDLIST\n"))
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(piece)
		}
		return true
	})
	prov := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/live/realU/realP/") && strings.HasSuffix(r.URL.Path, ".m3u8"):
			http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok"+channelID(r.URL.Path)+"/index.m3u8", 302)
		case strings.HasPrefix(r.URL.Path, "/timeshift/"):
			http.Redirect(w, r, cdn.srv.URL+"/arch/tok"+channelID(r.URL.Path)+"/index.m3u8", 302)
		default:
			return false
		}
		return true
	})
	_, base := limFront(t, prov, "hls", "2", advanced{})
	const recorder, viewer, third = "192.0.2.9", "192.0.2.1", "192.0.2.5"

	rec := ask(t, base, recorder, "/live/tv/easy/12.ts")
	defer rec.Body.Close()
	if rec.StatusCode != 200 {
		t.Fatalf("the recorder: %d", rec.StatusCode)
	}
	d := drain(rec)
	d.flowing(t, "the recording at its start", 3*time.Second)

	r := ask(t, base, viewer, "/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, viewer, loc)
	}
	text := body(r)
	var pieces []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "/__cdn/"); i >= 0 && !strings.HasPrefix(line, "#") {
			pieces = append(pieces, line[i:])
		}
	}
	if r.StatusCode != 200 || len(pieces) != 30 {
		t.Fatalf("the catch-up's playlist: %d %q", r.StatusCode, text)
	}
	for i := 0; i < 12; i++ { // (the third device's app asks again a few times, as apps do)
		p := ask(t, base, viewer, pieces[i]) // the catch-up is being watched
		p.Body.Close()
		if p.StatusCode != 200 {
			t.Fatalf("a piece of the catch-up: %d", p.StatusCode)
		}
		o := ask(t, base, third, "/live/tv/easy/20.m3u8")
		o.Body.Close()
		if o.StatusCode != 503 {
			t.Errorf("ask %d: both connections are in use by other devices (a recording and a catch-up), and a third device's channel got %d, want 503", i+1, o.StatusCode)
			break
		}
	}
	select {
	case <-d.done:
		t.Fatalf("the recording was ended")
	default:
	}
	d.flowing(t, "the recording, after a third device was turned away", 3*time.Second)
}
