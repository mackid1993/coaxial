package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// A reader that falls behind its channel is cut, so that it cannot hold memory without end. How far behind it may
// be was counted in bytes alone (16 MB for 15 s): 25 s of video on a 5 Mbit/s channel, but 6 s on a 20 Mbit/s
// one. There, a player that read nothing for 10 s (its user paused it, or its own pipeline started over) and then
// read on at playing speed was cut a quarter of a minute later, where the same player on a channel of a lower
// rate stays on. A device that never reads must still be cut, and nobody else slowed by it
// (TestLiveRoughFeedsPassStraightThrough has that case too).
func TestLiveReaderBehindOnAHighRateChannel(t *testing.T) {
	const rate = 2500000 // bytes a second: 20 Mbit/s
	const lump = 6       // seconds between the provider's lumps
	const pause = 10     // seconds the player reads nothing
	const id = "931"
	data := roughBytes(4*rate + 9*lump*rate)
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/"+id+".ts" {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		for off, n := 0, 4*rate; off < len(data); n = lump * rate { // an opening burst of 4 s, then a lump every 6 s
			n = min(n, len(data)-off)
			if _, err := w.Write(data[off : off+n]); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			off += n
			select {
			case <-time.After(lump * time.Second):
			case <-r.Context().Done():
				return true
			}
		}
		<-r.Context().Done()
		return true
	})
	log := captureLog(t)
	_, base := advFront(t, fp, "ts", advanced{})
	r := getAs(t, base+"/live/tv/easy/"+id+".ts", "192.0.2.50")
	if r.StatusCode != 200 {
		t.Fatalf("the channel: %d", r.StatusCode)
	}
	defer r.Body.Close()
	u, _ := url.Parse(base) // another device asks for the channel and never reads a byte of it
	idle, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	fmt.Fprintf(idle, "GET /live/tv/easy/%s.ts HTTP/1.1\r\nHost: %s\r\nX-Dev: 192.0.2.51\r\n\r\n", id, u.Host)

	start, got := time.Now(), 0
	buf := make([]byte, 256<<10)
	read := func(most int) error {
		k, err := r.Body.Read(buf[:most])
		if k > 0 && !bytes.Equal(buf[:k], data[got:got+k]) {
			t.Fatalf("the player was sent bytes the provider did not send, at byte %d", got)
		}
		got += k
		return err
	}
	for time.Since(start) < 9*time.Second { // it reads all it is sent, until a lump comes after 9 s...
		if err := read(len(buf)); err != nil {
			t.Fatalf("the player was cut while it read everything: %v", err)
		}
	}
	time.Sleep(pause * time.Second) // ...then nothing for 10 s...
	resumed := time.Now()
	for time.Since(resumed) < 20*time.Second { // ...then at playing speed: a tenth of a second of video every tenth
		tick := time.Now()
		for want := rate / 10; want > 0; {
			before := got
			if err := read(min(want, len(buf))); err != nil {
				t.Fatalf("a player that read nothing for %d s and then read on at playing speed was cut %.1f s after it read on (%d bytes sent to it): %v",
					pause, time.Since(resumed).Seconds(), got, err)
			}
			want -= got - before
		}
		time.Sleep(time.Until(tick.Add(100 * time.Millisecond)))
	}
	if n := len(linesWith(log.String(), "too slow to keep up", "192.0.2.50")); n != 0 {
		t.Errorf("the player was taken for too slow %d times", n)
	}
	if n := len(linesWith(log.String(), "too slow to keep up", "192.0.2.51")); n != 1 {
		t.Errorf("the device that never read was cut %d times in %.0f s, want once", n, time.Since(start).Seconds())
	}
}
