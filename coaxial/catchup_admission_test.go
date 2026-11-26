package main

// Catch-up admission: a device's parallel reads of one catch-up share one place (still counted after the first
// read ends), and its repeated asks reach the provider once.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cuProv: a provider that answers catch-up with a 302 to an archive server (a fresh token per ask, after delay),
// and the archive server, which streams until the reader hangs up (bad tokens answered with status bad).
func cuProv(t *testing.T, delay time.Duration, bad map[string]int) (*fakeProv, *fakeProv) {
	reset := func() { // (one test's answers are not another's: the tables are the program's own)
		mu.Lock()
		cuAsks, followUps = map[string]*cuAsk{}, map[string]*followUp{}
		for h := range holds {
			delete(holds, h)
		}
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/arch/") {
			if s := bad[r.URL.Query().Get("t")]; s != 0 {
				w.WriteHeader(s)
				return true
			}
			fp.stream(w, r, "arch", false, 0)
			return true
		}
		return false
	})
	var tok atomic.Int64
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/timeshift/") {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return true
			}
			id := strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/timeshift/"), "/", "_")
			http.Redirect(w, r, cdn.srv.URL+"/arch/"+id+"?t=tok"+string(rune('0'+tok.Add(1))), 302)
			return true
		}
		return false
	})
	return fp, cdn
}

const cu12 = "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts"

func redirect(t *testing.T, base, dev, p string) string {
	t.Helper()
	r := ask(t, base, dev, p)
	r.Body.Close()
	if r.StatusCode != 302 {
		t.Fatalf("%s %s: %d, want 302", dev, p, r.StatusCode)
	}
	return r.Header.Get("Location")
}

func pendingCuAsks() (pending, total int) {
	mu.Lock()
	defer mu.Unlock()
	for _, a := range cuAsks {
		total++
		if a.at == 0 {
			pending++
		}
	}
	return
}

// 1. A seek opens its read before the player closes the one it replaces (the main read). The seek joined the
// main read (shared), so once the main read closes it is counted nowhere: another device then gets the channel
// under one connection per channel, and the provider ends one of the two.
func TestAuditSharedReadOutlivesFirst(t *testing.T) {
	fp, cdn := cuProv(t, 0, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	loc := redirect(t, base, "192.0.2.1", cu12)
	main := ask(t, base, "192.0.2.1", loc, "Range", "bytes=0-")
	seek := ask(t, base, "192.0.2.1", loc, "Range", "bytes=50000000-")
	defer seek.Body.Close()
	if main.StatusCode != 200 || seek.StatusCode != 200 {
		t.Fatalf("archive reads: %d %d", main.StatusCode, seek.StatusCode)
	}
	if b := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts"); b.StatusCode != 503 {
		t.Errorf("control (main read still open): another device's live of the channel got %d, want 503", b.StatusCode)
		b.Body.Close()
	} else {
		b.Body.Close()
	}
	main.Body.Close() // (the player lets go of the read the seek replaced)
	eventually(t, func() bool { return cdn.count(cdn.open, "arch") == 1 }, "the main read closing")
	b := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
	if b.StatusCode != 503 {
		t.Errorf("FAULT: device A still reads its catch-up of channel 12 (archive reads open at the provider: %d), "+
			"yet another device's live of channel 12 got %d, want 503", cdn.count(cdn.open, "arch"), b.StatusCode)
	}
	b.Body.Close()
	c := ask(t, base, "192.0.2.9", "/timeshift/tv/easy/30/2026-01-01:09-00/12.ts")
	if c.StatusCode != 503 {
		t.Errorf("FAULT: another device's catch-up of channel 12 got %d, want 503", c.StatusCode)
	}
	c.Body.Close()
}

// 1b. The same with the connection limit: the remaining shared read is not counted, so one more stream is
// let through than the account allows.
func TestAuditSharedReadOutlivesFirstLimit(t *testing.T) {
	fp, cdn := cuProv(t, 0, nil)
	_, base := limFront(t, fp, "ts", "1", advanced{})
	loc := redirect(t, base, "192.0.2.1", cu12)
	main := ask(t, base, "192.0.2.1", loc, "Range", "bytes=0-")
	seek := ask(t, base, "192.0.2.1", loc, "Range", "bytes=50000000-")
	defer seek.Body.Close()
	if b := ask(t, base, "192.0.2.9", "/live/tv/easy/14.ts"); b.StatusCode != 503 {
		t.Errorf("control: %d", b.StatusCode)
	}
	main.Body.Close()
	eventually(t, func() bool { return cdn.count(cdn.open, "arch") == 1 }, "the main read closing")
	b := ask(t, base, "192.0.2.9", "/live/tv/easy/14.ts")
	if b.StatusCode != 503 {
		t.Errorf("FAULT: limit 1, a catch-up read open at the provider, another stream got %d (open at provider: %d)",
			b.StatusCode, fp.count(fp.open, "")+cdn.count(cdn.open, ""))
	}
	b.Body.Close()
}

// 2. The player's parallel asks of one catch-up address: one ask at the provider, all get its redirect.
// Hang-ups while waiting, a refused second device, and no entry left on its way afterwards.
func TestAuditCuAsksLifecycle(t *testing.T) {
	fp, cdn := cuProv(t, 300*time.Millisecond, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	for round := 0; round < 3; round++ {
		p := "/timeshift/tv/easy/60/2026-01-01:1" + string(rune('0'+round)) + "-00/12.ts"
		before := fp.count(fp.hits, "/timeshift/realU/realP/60/2026-01-01:1"+string(rune('0'+round))+"-00/12.ts")
		var wg sync.WaitGroup
		locs := make(chan string, 20)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i > 0 { // (the first ask is the one sent on; the player's others come just after it)
					time.Sleep(30 * time.Millisecond)
				}
				ctx := context.Background()
				if i%3 == 2 { // hangs up while it waits
					var c context.CancelFunc
					ctx, c = context.WithTimeout(ctx, 100*time.Millisecond)
					defer c()
				}
				req, _ := http.NewRequestWithContext(ctx, "GET", base+p, nil)
				req.Header.Set("X-Dev", "192.0.2.1")
				if resp, err := devClient.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode == 302 {
						locs <- resp.Header.Get("Location")
					}
				}
			}(i)
		}
		wg.Wait()
		close(locs)
		seen := map[string]bool{}
		for l := range locs {
			seen[l] = true
		}
		after := fp.count(fp.hits, "/timeshift/realU/realP/60/2026-01-01:1"+string(rune('0'+round))+"-00/12.ts")
		if after-before != 1 || len(seen) != 1 {
			t.Errorf("round %d: provider asked %d times, %d different redirects; want 1 and 1", round, after-before, len(seen))
		}
		var loc string
		for l := range seen {
			loc = l
		}
		rd := ask(t, base, "192.0.2.1", loc)
		if rd.StatusCode != 200 {
			t.Fatalf("archive read: %d", rd.StatusCode)
		}
		// another device, same catch-up address: refused, and its entry does not stay on its way
		if r := ask(t, base, "192.0.2.2", p); r.StatusCode != 503 {
			t.Errorf("round %d: another device, same catch-up: %d, want 503", round, r.StatusCode)
			r.Body.Close()
		} else {
			r.Body.Close()
		}
		rd.Body.Close()
		eventually(t, func() bool { return cdn.count(cdn.open, "arch") == 0 }, "archive read closing")
	}
	if pend, total := pendingCuAsks(); pend != 0 {
		t.Errorf("entries left on their way: %d of %d", pend, total)
	}
}

// 3. A provider slower than 5 s to answer the catch-up ask: the player's parallel asks stop waiting and ask the
// provider themselves, which (measured) ends the device's earlier catch-up read: the pre-fix failure.
func TestAuditSlowCatchUpAnswer(t *testing.T) {
	fp, _ := cuProv(t, 6*time.Second, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			r := ask(t, base, "192.0.2.1", cu12)
			codes[i] = r.StatusCode
			r.Body.Close()
		}(i)
	}
	wg.Wait()
	n := fp.count(fp.hits, "/timeshift/realU/realP/60/2026-01-01:10-00/12.ts")
	t.Logf("answers %v; provider asked %d times", codes, n)
	if n != 1 {
		t.Errorf("SLOW PROVIDER: the catch-up address was asked %d times at the provider, want 1", n)
	}
}

// 4. A rewind to another start time from the same device replaces its read (the old one ends).
func TestAuditRewindReplaces(t *testing.T) {
	fp, _ := cuProv(t, 0, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	loc := redirect(t, base, "192.0.2.1", cu12)
	main := ask(t, base, "192.0.2.1", loc)
	seek := ask(t, base, "192.0.2.1", loc, "Range", "bytes=1000-")
	loc2 := redirect(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:08-00/12.ts")
	if loc2 == loc {
		t.Fatalf("the rewind got the old redirect")
	}
	r2 := ask(t, base, "192.0.2.1", loc2)
	if r2.StatusCode != 200 {
		t.Fatalf("the rewound read: %d %q", r2.StatusCode, body(r2))
	}
	defer r2.Body.Close()
	if !ended(main) {
		t.Errorf("the old main read was not let go")
	}
	t.Logf("old shared (seek) read ended by Coaxial: %v (the provider ends it on the new ask anyway)", ended(seek))
}

// 5. The archive answers an error (a token the archive no longer takes): the player asks the catch-up address
// again within 30 s and gets the same address back.
func TestAuditStaleRedirect(t *testing.T) {
	fp, _ := cuProv(t, 0, map[string]int{"tok1": 403})
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	loc := redirect(t, base, "192.0.2.1", cu12)
	if r := ask(t, base, "192.0.2.1", loc); r.StatusCode != 403 {
		t.Fatalf("archive: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	again := redirect(t, base, "192.0.2.1", cu12)
	t.Logf("retry within 30 s: same redirect %v (provider asked %d times)", again == loc,
		fp.count(fp.hits, "/timeshift/realU/realP/60/2026-01-01:10-00/12.ts"))
}
