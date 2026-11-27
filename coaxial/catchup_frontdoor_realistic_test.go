package main

// Catch-up through the front door, on a provider that counts catch-up of a channel as that channel (one read
// per channel, newest wins) and lets one device read its archive file several ways at once. The stand-in
// provider is the one in vod_frontdoor_realistic_test.go.

import (
	"strings"
	"testing"
	"time"
)

const fdrCatchUp = "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts"

// A device watches catch-up of channel 12 and goes back to live, opening the live channel before it lets go
// of its catch-up read (as players do). Another device asking for channel 12 meanwhile is turned away: the
// provider would end the catch-up. The device's own live channel replaces its catch-up at once, the catch-up
// is let go at the provider before the live channel is asked for (so the provider ends nothing), and the
// catch-up's answer ends broken.
func TestFrontdoorBackToLiveFromCatchUp(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) { p.catchUpIsChannel, p.pace = true, 30*time.Millisecond })
	_, base := advFront(t, p.own, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	cu := fdrAsk(t, "GET", base, "192.0.2.61", fdrCatchUp, "")
	if cu.resp.StatusCode != 200 {
		t.Fatalf("the catch-up: %d", cu.resp.StatusCode)
	}
	other := ask(t, base, "192.0.2.62", "/live/tv/easy/12.ts")
	if text := body(other); other.StatusCode != 503 || !strings.Contains(text, "one stream per channel") {
		t.Errorf("another device's live channel 12 while its catch-up plays: %d %s, want 503", other.StatusCode, fdrShort(text))
	}
	if n := p.count(func(p *fdrProv) int { return p.asks["/live/realU/realP/12.ts"] }); n != 0 {
		t.Fatalf("the provider was asked for the refused live channel")
	}
	if over, _ := cu.ended(0); over {
		t.Fatalf("the catch-up is over before the device goes back to live (%d bytes): the test's file is sent too fast", cu.n())
	}
	live := fdrAsk(t, "GET", base, "192.0.2.61", "/live/tv/easy/12.ts", "")
	if live.resp.StatusCode != 200 {
		t.Fatalf("back to live: %d %q", live.resp.StatusCode, body(live.resp))
	}
	if live.took > 1500*time.Millisecond {
		t.Errorf("back to live was answered after %.1f s: the catch-up it replaces held it up", live.took.Seconds())
	}
	for end := time.Now().Add(1500 * time.Millisecond); live.n() < 188*100 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := live.n(); n < 188*100 {
		t.Errorf("back to live: %d bytes of video in the first 1.5 s (answered after %.1f s)", n, live.took.Seconds())
	}
	if over, err := cu.ended(2 * time.Second); !over || err == nil {
		t.Errorf("the catch-up the live channel replaced: ended %v, error %v; want it cut, and broken", over, err)
	}
	if k := p.count(func(p *fdrProv) int { return p.kicked }); k != 0 {
		t.Errorf("the provider had to end %d read(s): the catch-up was still open there when the live channel was asked for", k)
	}
}

// The provider takes 3 s to start a catch-up (it builds the archive file). The player opens its main read,
// and a second later a read of the file's end (for its length) at the same address, before the first is
// answered. The second read joins the first: neither is cut, and both get their bytes. Another device's live
// channel 12 is turned away all the while.
func TestFrontdoorCatchUpReadsSideBySideDuringASlowStart(t *testing.T) {
	p := newFdrProv(t)
	rp := "/timeshift/realU/realP/60/2026-01-01:10-00/12.ts"
	p.set(func(p *fdrProv) {
		p.catchUpIsChannel = true
		p.plans[rp] = []fdrPlan{{start: 3 * time.Second}, {start: 3 * time.Second}}
	})
	_, base := advFront(t, p.own, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	type answer struct {
		x   *fdrReading
		err error
	}
	start := func(rg string) chan answer {
		c := make(chan answer, 1)
		go func() { x, err := fdrTry("GET", base, "192.0.2.71", fdrCatchUp, rg); c <- answer{x, err} }()
		return c
	}
	mainC := start("")
	time.Sleep(time.Second)
	endC := start("bytes=-188000")
	time.Sleep(500 * time.Millisecond)
	other := ask(t, base, "192.0.2.72", "/live/tv/easy/12.ts")
	if text := body(other); other.StatusCode != 503 {
		t.Errorf("another device's live channel 12 while the catch-up starts: %d %s, want 503", other.StatusCode, fdrShort(text))
	}
	m, e := <-mainC, <-endC
	for _, a := range []answer{m, e} {
		if a.err != nil {
			t.Fatalf("a read of the catch-up failed: %v", a.err)
		}
		defer a.x.resp.Body.Close()
	}
	if m.x.resp.StatusCode != 200 || e.x.resp.StatusCode != 206 {
		t.Fatalf("the main read: %d, the read of the end: %d; want 200 and 206", m.x.resp.StatusCode, e.x.resp.StatusCode)
	}
	fdrSame(t, "the main read", m.x, p.film, 0, 10*time.Second)
	fdrSame(t, "the read of the file's end", e.x, p.film, fdrSize-188000, 10*time.Second)
	if k := p.count(func(p *fdrProv) int { return p.kicked }); k != 0 {
		t.Errorf("the provider had to end %d read(s)", k)
	}
	if n := p.count(func(p *fdrProv) int { return p.asks["/live/realU/realP/12.ts"] }); n != 0 {
		t.Errorf("the provider was asked for the refused live channel")
	}
}
