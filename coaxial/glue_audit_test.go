package main

import (
	"testing"
	"time"
)

// A live channel's admission hold ends once the channel counts it. A player that opened a channel and paused
// (it reads no more, its handler stays on) is one connection at the provider, not two: at an account that
// allows 2, another device still opens a channel of its own.
func TestLiveHoldNotKeptAfterOpen(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := limFront(t, fp, "ts", "2", advanced{})
	a := ask(t, base, "192.0.2.1", "/live/tv/easy/931.ts") // read a little, then not read: a paused player
	defer a.Body.Close()
	time.Sleep(time.Second)
	b := ask(t, base, "192.0.2.2", "/live/tv/easy/932.ts")
	defer b.Body.Close()
	if b.StatusCode != 200 {
		t.Errorf("another device at an account that allows 2, one channel open: %d", b.StatusCode)
	}
	if n := fp.count(fp.most, ""); n != 2 {
		t.Errorf("the provider saw %d streams at once, want 2", n)
	}
}

func TestLiveDroppedChannelLeavesNoPhantom(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	f, base := limFront(t, fp, "ts", "1", advanced{})
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
