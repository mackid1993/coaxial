package main

import (
	"testing"
	"time"
)

// A live tune well under the account's limit, on a channel nothing else holds, is opened at once: it does not
// go through the rules (admit) at all. Near the limit, or with one_per_channel and the same channel held
// elsewhere, it still does.
func TestLiveTuneUnderLimitSkipsRules(t *testing.T) {
	on := true
	fp := newFakeProv(t, false, nil)
	f, base := advFront(t, fp, "ts", advanced{StayUnder: &on, Connections: "5", OnePerChannel: true})
	tv := ask(t, base, "192.0.2.5", "/live/tv/easy/11.ts")
	defer tv.Body.Close()
	if tv.StatusCode != 200 {
		t.Fatalf("tuning: %d", tv.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	other, sameCh := f.needsRules(what{"ch", "12"}), f.needsRules(what{"ch", "11"})
	mu.Unlock()
	if other {
		t.Errorf("a tune to another channel at 1 of 5 goes through the rules")
	}
	if !sameCh {
		t.Errorf("with one_per_channel, a channel already held does not go through the rules")
	}
	zap := ask(t, base, "192.0.2.5", "/live/tv/easy/12.ts") // changing channel
	defer zap.Body.Close()
	if zap.StatusCode != 200 {
		t.Fatalf("changing channel: %d %q", zap.StatusCode, body(zap))
	}

	fp2 := newFakeProv(t, false, nil)
	g, base2 := advFront(t, fp2, "ts", advanced{StayUnder: &on, Connections: "2"})
	one := ask(t, base2, "192.0.2.6", "/live/tv/easy/21.ts")
	defer one.Body.Close()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	near := g.needsRules(what{"ch", "22"})
	mu.Unlock()
	if !near {
		t.Errorf("a tune that reaches the limit does not go through the rules")
	}
}
