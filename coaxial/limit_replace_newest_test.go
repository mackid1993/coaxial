package main

import (
	"testing"
	"time"
)

// A media server is one device that records and serves a viewer. At the limit, the viewer changing channel
// (the new channel asked for before the old one is hung up) replaces the stream it is leaving, the newest of
// the device's own, not the recording that has run the longest.
func TestLimitReplacesNewestOwnStream(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{Connections: "2"})
	rec := ask(t, base, "192.0.2.5", "/live/tv/easy/11.ts") // recording
	defer rec.Body.Close()
	time.Sleep(50 * time.Millisecond)
	view := ask(t, base, "192.0.2.5", "/live/tv/easy/12.ts") // a viewer through the same server
	defer view.Body.Close()
	if rec.StatusCode != 200 || view.StatusCode != 200 {
		t.Fatalf("recording %d, viewing %d", rec.StatusCode, view.StatusCode)
	}
	zap := ask(t, base, "192.0.2.5", "/live/tv/easy/13.ts") // the viewer changes channel
	defer zap.Body.Close()
	if zap.StatusCode != 200 {
		t.Fatalf("changing channel at the limit: %d %q", zap.StatusCode, body(zap))
	}
	time.Sleep(300 * time.Millisecond)
	if fp.count(fp.open, "ch11") != 1 {
		t.Errorf("the recording was cut to make room for the viewer's new channel")
	}
	if fp.count(fp.open, "ch12") != 0 {
		t.Errorf("the channel the viewer left is still open")
	}
}

// At the limit, a device seeking in a film replaces its own read of that film, not the other stream the same
// device holds (a recording started after the film).
func TestLimitSeekReplacesSameFilm(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{Connections: "2"})
	film := ask(t, base, "192.0.2.5", "/movie/tv/easy/7.mkv")
	defer film.Body.Close()
	time.Sleep(50 * time.Millisecond)
	rec := ask(t, base, "192.0.2.5", "/live/tv/easy/11.ts")
	defer rec.Body.Close()
	if film.StatusCode != 200 || rec.StatusCode != 200 {
		t.Fatalf("film %d, recording %d", film.StatusCode, rec.StatusCode)
	}
	seek := ask(t, base, "192.0.2.5", "/movie/tv/easy/7.mkv", "Range", "bytes=1000000-")
	defer seek.Body.Close()
	if seek.StatusCode != 200 {
		t.Fatalf("seeking at the limit: %d %q", seek.StatusCode, body(seek))
	}
	time.Sleep(300 * time.Millisecond)
	if fp.count(fp.open, "ch11") != 1 {
		t.Errorf("the recording was cut to make room for a seek in the film")
	}
	if fp.count(fp.open, "vod7") != 1 {
		t.Errorf("the film is open %d times, want 1", fp.count(fp.open, "vod7"))
	}
}
