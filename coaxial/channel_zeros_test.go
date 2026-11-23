package main

import (
	"strings"
	"testing"
)

// A channel number written with leading zeros is the same channel at the provider (Xtream servers read it
// as a number): live or catch-up of 012 while 12 is open is a second stream of it.
func TestOnePerChannelLeadingZeros(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	live := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
	if live.StatusCode != 200 {
		t.Fatalf("live: %d", live.StatusCode)
	}
	defer live.Body.Close()
	for _, p := range []string{"/live/tv/easy/012.ts", "/timeshift/tv/easy/60/2026-01-01:10-00/012.ts",
		"/streaming/timeshift.php?username=tv&password=easy&stream=0012&start=2026-01-01:10-00&duration=60"} {
		r := ask(t, base, "192.0.2.2", p)
		if r.StatusCode != 503 {
			r.Body.Close()
			t.Errorf("another device, %s while channel 12 is open: %d", p, r.StatusCode)
		} else if text := body(r); !strings.Contains(text, "one stream per channel") {
			t.Errorf("another device, %s while channel 12 is open: %q", p, text)
		}
	}
}
