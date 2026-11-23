package main

import (
	"testing"
	"time"
)

// A live channel's admission hold ends once the channel counts it. A first reader still connected after its
// channel was let go (a paused player whose queue has not drained) must not count as a second connection.
func TestLiveHoldNotKeptAfterOpen(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	f, base := advFront(t, fp, "ts", advanced{Connections: "2"})
	a := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts") // read a little, then not read: a paused player
	defer a.Body.Close()
	mu.Lock()
	n := 0
	for h := range holds {
		if h.f == f && !h.shared && h.until == 0 {
			n++
		}
	}
	mu.Unlock()
	if n != 0 {
		t.Errorf("%d live admission hold(s) still held while the channel is open", n)
	}
}

func TestLiveDroppedChannelLeavesNoPhantom(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	f, base := advFront(t, fp, "ts", advanced{Connections: "1"})
	a := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts") // a paused player: its handler stays on
	defer a.Body.Close()
	time.Sleep(5 * time.Second) // (its queue fills)
	mu.Lock()
	for _, ch := range channels {
		if ch.provider == f.base {
			ch.drop() // let go, as a cut to make room does
		}
	}
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	for h := range holds {
		if h.f == f {
			t.Logf("after the drop: hold %q shared=%v cutAt=%v open=%v", h.key, h.shared, h.cutAt, open(h.key))
		}
	}
	mu.Unlock()
	start := time.Now()
	b := ask(t, base, "192.0.2.2", "/live/tv/easy/13.ts")
	took := time.Since(start)
	defer b.Body.Close()
	t.Logf("took %v", took)
	if b.StatusCode != 200 {
		t.Errorf("another device, nothing open at the provider: %d %q (after %v)", b.StatusCode, body(b), took)
	}
}
