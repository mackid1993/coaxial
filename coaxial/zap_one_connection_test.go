package main

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Changing channel, again and again, on an account that allows one connection: what a viewer with a remote
// control does all evening, most often on a player that reads MPEG-TS. Each change is a hang-up on the old
// channel and an ask for the new one, in either order (some players open the new channel before they let go
// of the old one), and some asks are given up after 30 or 180 ms, answered or not (the viewer is already on
// the next channel). The tests of the limit (limit_replace_newest_test.go and the rest) take one change at a time.
// Here one device makes sixty in a row, as fast as it is answered. What must hold:
//
//   1. every channel the viewer waits for plays: it is never turned away for the connection its own last
//      channel still holds, and its video is that channel's;
//   2. the provider never has two connections open at once for longer than the moment a hang-up takes to
//      arrive (providers end the older one or refuse the newer, and count either against the account);
//   3. when the viewer stops, nothing is left: no connection at the provider, no channel and nothing counted
//      in the proxy, so another device can use the account at once.

// zapOverlap: how long the provider may see the old channel and the new one open together. The proxy lets go
// of the old one and gives the provider dropGap (100 ms) to see it before it asks for the new one; what is
// measured here is the stand-in's own delay in noticing a hang-up on top of that (a few ms, more on a test
// machine that stands still for a moment). An old channel that is not let go of stays open for good.
const zapOverlap = time.Second

type zapConn struct{ from, to time.Time }

func TestZappingOnAnAccountWithOneConnection(t *testing.T) {
	var cmu sync.Mutex
	var conns []*zapConn
	open := 0
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") {
			return false
		}
		c := &zapConn{from: time.Now()}
		cmu.Lock()
		conns = append(conns, c)
		open++
		cmu.Unlock()
		defer func() {
			cmu.Lock()
			c.to = time.Now()
			open--
			cmu.Unlock()
		}()
		lump := make([]byte, 188*50) // the channel's own video: every packet names the channel
		for i := 0; i < len(lump); i += 188 {
			lump[i], lump[i+1], lump[i+2], lump[i+3] = 0x47, 0x1f, 0xff, 0x10
			binary.BigEndian.PutUint32(lump[i+4:], uint32(atoi(channelID(r.URL.Path))))
		}
		w.Header().Set("Content-Type", "video/mp2t")
		for {
			if _, err := w.Write(lump); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	f, base := limFront(t, fp, "ts", "1", advanced{})
	const dev, zaps = "192.0.2.1", 60
	var held *http.Response // the channel the player has not hung up on yet
	played, given := 0, 0
	for i := 0; i < zaps; i++ {
		id := 100 + i
		req, _ := http.NewRequest("GET", base+"/live/tv/easy/"+itoa(id)+".ts", nil)
		req.Header.Set("X-Dev", dev)
		if i%4 == 2 { // given up at once, answered or not: the viewer is already on the next channel
			patience := 30 * time.Millisecond // (before the provider was asked)
			if i%8 == 6 {
				patience = 180 * time.Millisecond // (after it was)
			}
			ctx, cancel := context.WithTimeout(context.Background(), patience)
			if resp, err := devClient.Do(req.WithContext(ctx)); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			cancel()
			given++
			continue
		}
		done := within(t, 4*time.Second, "channel "+itoa(id))
		began := time.Now()
		resp, err := devClient.Do(req)
		done()
		if held != nil { // (the player that opens the new channel first lets go of the old one now)
			held.Body.Close()
			held = nil
		}
		if err != nil {
			t.Fatalf("change %d, to channel %d: %v", i+1, id, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("change %d, to channel %d: the viewer was answered %d %q; its own last channel is all that could be in the way, and it has left it",
				i+1, id, resp.StatusCode, strings.TrimSpace(body(resp)))
		}
		want := 4 << 10
		if i%4 == 0 {
			want = 200 << 10
		}
		video := make([]byte, want)
		if _, err := io.ReadFull(resp.Body, video); err != nil || video[0] != 0x47 || int(binary.BigEndian.Uint32(video[4:])) != id {
			t.Fatalf("change %d, to channel %d: the video is not that channel's (%v; its first packet names channel %d)", i+1, id, err, binary.BigEndian.Uint32(video[4:]))
		}
		if took := time.Since(began); took > 4*time.Second {
			t.Errorf("change %d, to channel %d: video after %.1f s", i+1, id, took.Seconds())
		}
		played++
		if i%4 == 3 { // this player opens its next channel before it hangs up on this one
			held = resp
		} else {
			resp.Body.Close()
		}
	}
	if held != nil {
		held.Body.Close()
	}
	// Nothing is left.
	gone := false
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && !gone; time.Sleep(20 * time.Millisecond) {
		cmu.Lock()
		gone = open == 0
		cmu.Unlock()
	}
	if !gone {
		t.Errorf("3 s after the viewer's last hang-up the provider still has a connection open")
	}
	left := func() (chans, counted int) {
		mu.Lock()
		defer mu.Unlock()
		for _, ch := range channels {
			if ch.provider == f.base && !ch.done {
				chans++
			}
		}
		return chans, len(f.conns())
	}
	var chans, counted int
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if chans, counted = left(); chans == 0 && counted == 0 {
			break
		}
	}
	if chans != 0 || counted != 0 {
		t.Errorf("after %d channel changes and the last hang-up the proxy still has %d channels open and counts %d connections in use", zaps, chans, counted)
	}
	other := ask(t, base, "192.0.2.2", "/live/tv/easy/999.ts")
	if other.StatusCode != 200 {
		t.Errorf("another device asked for a channel once the viewer had stopped: %d %q", other.StatusCode, strings.TrimSpace(body(other)))
	} else {
		other.Body.Close()
	}
	// The provider's side: one connection at a time.
	cmu.Lock()
	defer cmu.Unlock()
	sort.Slice(conns, func(i, j int) bool { return conns[i].from.Before(conns[j].from) })
	worst, pairs := time.Duration(0), 0
	for i := 1; i < len(conns); i++ {
		if conns[i-1].to.IsZero() {
			continue
		}
		if d := conns[i-1].to.Sub(conns[i].from); d > 0 {
			pairs++
			worst = max(worst, d)
		}
	}
	t.Logf("%d channel changes: %d played, %d given up at once; the provider was asked %d times; two connections were open together %d times, for %d ms at most",
		zaps, played, given, len(conns), pairs, worst.Milliseconds())
	if worst > zapOverlap {
		t.Errorf("the provider had the old channel and the new one open together for %d ms; the account allows one connection (at most %d ms is taken for the stand-in's own delay)",
			worst.Milliseconds(), zapOverlap.Milliseconds())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
