package main

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Two meanings of the connections typed in, kept apart. "The account allows N" is the user's fact about the
// account (f.bought) and holds whatever stay_under_limit says; "Coaxial keeps the account under N" is the
// option (f.limit(), 0 when it is off). The rules that ask whether opening another channel can end this one
// (tsSource's single, reaskAllowed, releaseFor) need the first: with the option off, limit() is 0 and they took a
// two-connection account for a one-connection one. The refusal's wording needs the second.

// A device on HLS built from MPEG-TS changes channel on an account that allows one connection, or with no
// number typed in: its old channel's builder and provider connection are let go at once (the provider would
// otherwise see two), with stay_under_limit on or off. Guards releaseFor's other side: it reads the number typed
// in, and only two or more keeps the old channel.
func TestHLSFromTSChannelChangeFreesOldOnOneConnection(t *testing.T) {
	for _, c := range []struct {
		name        string
		connections number
		a           advanced
	}{{"no number", "", advanced{}}, {"no number, stay_under_limit off", "", advanced{StayUnder: off()}},
		{"one connection", "1", advanced{}}, {"one connection, stay_under_limit off", "1", advanced{StayUnder: off()}}} {
		t.Run(c.name, func(t *testing.T) {
			p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: 34, clock: 1000}), burst: 14,
				asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
			fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") {
					return false
				}
				p.serve(w, r)
				return true
			})
			_, base := limFront(t, fp, "ts", c.connections, c.a)
			const dev = "192.0.2.73"
			if st, _, _, _ := tsHLSGet(t, dev, base+"/live/tv/easy/7.m3u8"); st != 200 {
				t.Fatalf("channel 7 as HLS: %d", st)
			}
			if st, _, _, _ := tsHLSGet(t, dev, base+"/live/tv/easy/8.m3u8"); st != 200 {
				t.Fatalf("channel 8 as HLS: %d", st)
			}
			eventually(t, func() bool {
				p.mu.Lock()
				defer p.mu.Unlock()
				return p.open["7"] == 0
			}, "the old channel's provider connection closing after the change (it waits for segIdle when it is not let go)")
			p.mu.Lock()
			a7, a8 := p.asks["7"], p.asks["8"]
			p.mu.Unlock()
			if a7 != 1 || a8 != 1 {
				t.Errorf("the provider was asked for channel 7 %d times and channel 8 %d times, want once each", a7, a8)
			}
		})
	}
}

// reaskCase (reask_other_line_test.go) with stay_under_limit on and off: a channel opened on the recorder's own
// account a few seconds before the provider ends the recorder's channel. On an account that allows two, the
// recorder's channel is asked again either way; with one or none typed in, it is not (asking would end the
// channel just opened), the option off included.
func TestReaskAfterAnotherChannelOpenedCountsTheNumberTypedIn(t *testing.T) {
	stayOff := advanced{StayUnder: off()}
	t.Run("two connections: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "2", nil, 200, 2)
	})
	t.Run("two connections, stay_under_limit off: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "2", nil, 200, 2, stayOff)
	})
	t.Run("two connections, stay_under_limit off, opened as HLS: asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "2", nil, 0, 2, stayOff)
	})
	t.Run("one connection, stay_under_limit off: not asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "1", nil, 200, 1, stayOff)
	})
	t.Run("no number, stay_under_limit off: not asked again", func(t *testing.T) {
		t.Parallel()
		reaskCase(t, "", nil, 200, 1, stayOff)
	})
}

// The extra asks after a drop (reaskAllowed): the provider ends the recorder's channel after reopenAfter, answers
// the first ask again with 404 (it is restarting the channel), and plays again from the next one. Another device
// opens a channel on the same account between the two asks. On an account that allows two, the recorder's channel
// is still asked again and the recording carries on, with stay_under_limit on or off; with one or none typed in,
// it is not asked again (it would end the channel just opened), the option off included.
func TestReaskAfter404CountsTheNumberTypedIn(t *testing.T) {
	for _, c := range []struct {
		name        string
		connections number
		a           advanced
		asks        int
	}{
		{"two connections: asked again", "2", advanced{}, 3},
		{"two connections, stay_under_limit off: asked again", "2", advanced{StayUnder: off()}, 3},
		{"no number: not asked again", "", advanced{}, 2},
		{"no number, stay_under_limit off: not asked again", "", advanced{StayUnder: off()}, 2},
		{"one connection, stay_under_limit off: not asked again", "1", advanced{StayUnder: off()}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var asked atomic.Int32
			fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/live/realU/realP/12.ts" {
					return false
				}
				n := asked.Add(1)
				if n == 2 { // (restarting the channel)
					http.Error(w, "not found", 404)
					return true
				}
				w.Header().Set("Content-Type", "video/mp2t")
				chunk := make([]byte, 188*100)
				for i := 0; i < len(chunk); i += 188 {
					chunk[i] = 0x47
				}
				run := secs(reopenAfter + 2) // the first: ended a little after reopenAfter
				if n > 2 {
					run = 20 * time.Second
				}
				for end := time.Now().Add(run); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
					if _, err := w.Write(chunk); err != nil {
						return true
					}
					w.(http.Flusher).Flush()
				}
				return true
			})
			_, base := limFront(t, fp, "ts", c.connections, c.a)
			began := time.Now()
			rec := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
			if rec.StatusCode != 200 {
				t.Fatalf("the recorder: %d", rec.StatusCode)
			}
			defer rec.Body.Close()
			var got atomic.Int64
			var ended atomic.Bool
			go func() {
				buf := make([]byte, 65536)
				for {
					n, err := rec.Body.Read(buf)
					got.Add(int64(n))
					if err != nil {
						ended.Store(true)
						return
					}
				}
			}()
			// The drop is at about reopenAfter + 2 s, the 404 right after; the next ask about 2 s later.
			time.Sleep(time.Until(began.Add(secs(reopenAfter + 2.9))))
			if n := asked.Load(); n != 2 {
				t.Fatalf("the recorder's channel was asked for %d time(s) before the other device's tune, want 2 (the drop and the 404)", n)
			}
			tv := ask(t, base, "192.0.2.2", "/live/tv/easy/13.ts")
			if tv.StatusCode != 200 {
				t.Fatalf("the other device's channel: %d %q", tv.StatusCode, body(tv))
			}
			defer tv.Body.Close()
			go io.Copy(io.Discard, tv.Body)
			time.Sleep(time.Until(began.Add(secs(reopenAfter + 7))))
			before := got.Load()
			time.Sleep(time.Second)
			n, carries := int(asked.Load()), !ended.Load() && got.Load() > before
			if n != c.asks {
				t.Errorf("the recorder's channel was asked for %d time(s), want %d", n, c.asks)
			}
			if c.asks > 2 && !carries {
				t.Errorf("the recorder gets no more video (its stream ended: %v): the channel was not asked for again after the 404", ended.Load())
			}
			if c.asks == 2 && carries {
				t.Errorf("the recorder's channel was asked for again after another channel opened on a one-connection account")
			}
		})
	}
}

// The refusal for the limit names a number only while Coaxial keeps the account under it: with stay_under_limit
// off, the number typed in is still there but nothing was refused for it (a wait for a cut stream ran out), so
// the words must not say "All 2 connections ... are in use".
func TestRefusalNamesTheNumberOnlyWhileTheLimitIsKept(t *testing.T) {
	for _, c := range []struct {
		name     string
		bought   int
		on       bool
		want, no string
	}{
		{"two, kept", 2, true, "All 2 connections your provider allows are in use", "has not closed"},
		{"one, kept", 1, true, "The one connection your provider allows is in use", "has not closed"},
		{"two, stay_under_limit off", 2, false, "has not closed", "All 2"},
		{"one, stay_under_limit off", 1, false, "has not closed", "The one connection"},
		{"none, kept", 0, true, "has not closed", "All 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &front{name: "P", base: "http://198.51.100.21:80", bought: c.bought, limitOn: c.on}
			r := f.refusal("192.0.2.41", what{"vod", "9"}, "limit")
			if r == nil || !strings.Contains(string(r.text), c.want) || strings.Contains(string(r.text), c.no) {
				t.Errorf("the refusal reads %q, want it to say %q and not %q", r.text, c.want, c.no)
			}
		})
	}
}
