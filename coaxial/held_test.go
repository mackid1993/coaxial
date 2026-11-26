package main

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// When a streaming test fails, these helpers say where in the program the video was at that moment, as file:line
// with the function, so the line that holds it can be read in the test output. Three of them:
//
//   whereOnFail (every test that builds a front door with limFront or advFront has it): if the test failed, where
//   each part of the program was when the test ended.
//
//   lateFor (a test whose provider and device can both be counted): the first time bytes the provider has sent
//   are not with the device in time, where each part of the program was at that moment.
//
//   diffAt (a test that compares bytes): where the bytes first differ, and what was there.
//
// None of them fails a test or waits for anything; a test that passes does not run them at all.

var frameRe = regexp.MustCompile(`^\t(\S+\.go):(\d+)`)

// parked: one part of the program (a goroutine) and where it is.
type parked struct {
	state string   // what Go says it is doing: sleep, select, chan receive, IO wait ...
	in    string   // the call it is in, if that is not the program's own
	own   []string // the program's own lines it got there through, the innermost first
	by    string   // the program's own line that started it
	n     int      // how many parts are at this very place
}

// parkedNow: every part of the program that is somewhere in the program's own files (not the tests, not Go's
// own code). The log's own writer is left out: it waits for lines all day and holds nothing.
func parkedNow() []*parked {
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(self)
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []*parked
	same := map[string]*parked{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		lines := strings.Split(g, "\n")
		if len(lines) < 3 {
			continue
		}
		p := &parked{state: lines[0], n: 1}
		if i := strings.Index(p.state, "["); i >= 0 {
			p.state = p.state[i+1:]
		}
		if i := strings.IndexAny(p.state, ",]"); i >= 0 { // ("[select, 2 minutes]:")
			p.state = p.state[:i]
		}
		fn, first, logOnly := "", "", true
		for _, l := range lines[1:] {
			m := frameRe.FindStringSubmatch(l)
			if m == nil {
				fn = strings.TrimSpace(l)
				if i := strings.LastIndex(fn, "("); i > 0 {
					fn = fn[:i]
				}
				if first == "" {
					first = fn
				}
				continue
			}
			file := filepath.Base(m[1])
			if filepath.Dir(m[1]) != dir || strings.HasSuffix(file, "_test.go") {
				if len(p.own) == 0 {
					p.in = fn
				}
				continue
			}
			logOnly = logOnly && file == "logging.go"
			if by, ok := strings.CutPrefix(fn, "created by "); ok {
				by, _, _ = strings.Cut(by, " in goroutine")
				p.by = fmt.Sprintf("%s:%s  started by %s", file, m[2], by)
				continue
			}
			p.own = append(p.own, fmt.Sprintf("%s:%s  %s", file, m[2], fn))
		}
		if (len(p.own) == 0 && p.by == "") || logOnly {
			continue
		}
		if len(p.own) == 0 {
			p.in = first
		}
		key := p.state + "\n" + strings.Join(p.own, "\n") + "\n" + p.by
		if q := same[key]; q != nil {
			q.n++
			continue
		}
		same[key] = p
		out = append(out, p)
	}
	return out
}

// holdRank: whether a part in this state holds on its own (the lower the number, the more likely it is the cause).
// A sleep, a send on a full channel and a wait for a signal hold whatever comes after them. A part that waits
// for a lock is held by another part, which is the one to look for.
func holdRank(state string) int {
	for i, s := range []string{"sleep", "chan send", "sync.Cond", "sync.Mutex", "sync.RWMutex", "semacquire"} {
		if strings.HasPrefix(state, s) {
			return i
		}
	}
	return -1
}

func (p *parked) text(frames int) string {
	how := "waiting"
	switch {
	case holdRank(p.state) >= 3:
		how = "waiting for a lock another part holds"
	case p.state == "running" || p.state == "runnable" || p.state == "syscall":
		how = "at work"
	}
	s := fmt.Sprintf("    %s (%s)", how, p.state)
	if p.n > 1 {
		s += fmt.Sprintf(", %d of them,", p.n)
	}
	if p.in != "" {
		s += " in " + p.in + ", called from"
	} else {
		s += " at"
	}
	own := p.own
	if len(own) > frames {
		own = own[:frames]
	}
	if p.by != "" && (frames > 2 || len(own) == 0) {
		own = append(own[:len(own):len(own)], p.by)
	}
	return s + "\n        " + strings.Join(own, "\n        ")
}

// whereItIs: where each part of the program is right now. First the parts that hold on their own: asleep, stuck
// on a lock, stuck sending on a full channel. One of those is the likely cause when video is held. Then at
// most three that wait for another part (a channel, a select). Parts that only wait on the network for the
// provider or the player to send or take bytes are left out: that is not holding video.
func whereItIs() string { return whereItIsNow(false) }

// whereItIsNow: whereItIs; network: with at most three of the parts that wait on the network as well (when
// nothing came at all, the wait that is too long may be one on a connection).
func whereItIsNow(network bool) string {
	var hold, also, net []*parked
	for _, p := range parkedNow() {
		switch {
		case p.state == "IO wait" || strings.HasPrefix(p.in, "net/http.") || strings.HasPrefix(p.in, "net."):
			net = append(net, p) // (in Go's own network code: a request sent, an answer or a body awaited)
		case holdRank(p.state) >= 0:
			hold = append(hold, p)
		default:
			also = append(also, p)
		}
	}
	sort.SliceStable(hold, func(a, b int) bool { return holdRank(hold[a].state) < holdRank(hold[b].state) })
	busy := func(p *parked) bool { return !strings.HasPrefix(p.state, "chan") && p.state != "select" }
	sort.SliceStable(also, func(a, b int) bool { return !busy(also[a]) && busy(also[b]) })
	if len(hold) == 0 && len(also) == 0 && !network {
		return "  (no part of the program was waiting on anything but the network)"
	}
	var b strings.Builder
	list := func(head string, ps []*parked, most, frames int) {
		if len(ps) == 0 {
			return
		}
		b.WriteString("  " + head + "\n")
		for i, p := range ps {
			if i == most {
				fmt.Fprintf(&b, "    (and %d more)\n", len(ps)-most)
				break
			}
			b.WriteString(p.text(frames) + "\n")
		}
	}
	if len(hold) == 0 {
		b.WriteString("  No part of the program was asleep, or stuck on a lock or on a full channel.\n")
	}
	list("Likely cause (asleep, or stuck on a lock or on a full channel):", hold, 6, 4)
	list("Also waiting (for another part of the program):", also, 3, 2)
	if network {
		list("Waiting on the network (for a provider or a player):", net, 3, 2)
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	whereMu   sync.Mutex
	whereSaid = map[string]bool{}       // tests whose end has been written down
	lateBy    = map[string]*lateWatch{} // test -> its watch (lateFor)
)

// whereIfFailed: if the test has failed, where each part of the program is now goes into its output (once per
// test). For a setup's own teardown: call it before the servers are closed and the channels dropped.
func whereIfFailed(t *testing.T) {
	if !t.Failed() {
		return
	}
	whereMu.Lock()
	said := whereSaid[t.Name()]
	whereSaid[t.Name()] = true
	whereMu.Unlock()
	if !said {
		t.Logf("the test failed. Where the program was when it ended:\n%s", whereItIsNow(true))
	}
}

// whereOnFail: whereIfFailed when the test ends. Cleanups run last one first: register this AFTER the setup's
// own cleanup, so it looks before that one closes the servers and drops the channels.
func whereOnFail(t *testing.T) { t.Cleanup(func() { whereIfFailed(t) }) }

// heldNote: where the program was when video was seen to be held.
type heldNote struct {
	mu sync.Mutex
	s  string
}

func (a *heldNote) Store(s string) { a.mu.Lock(); a.s = s; a.mu.Unlock() }
func (a *heldNote) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.s }

func (a *heldNote) add(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.s != "" {
		a.s += "\n\n"
	}
	a.s += s
}

type lateMark struct {
	at  time.Time
	off int // bytes sent by then, in all
}

// lateWatch: the clock "the provider sent it at T, the device must have it by T+limit", kept while a test
// runs. The provider's side tells it what it has sent (sent, sentMore), the device's side what it has (got,
// gotMore, or reads around the answer's body). The first time bytes are late, where each part of the program
// is at that moment is written down in note. It fails nothing: the test's own checks do that.
type lateWatch struct {
	mu      sync.Mutex
	limit   time.Duration
	owed    []lateMark // what the provider has sent and the device does not have yet
	all     int        // sent in all
	have    int        // with the device
	last    time.Time  // when the device last got bytes
	reading bool       // a device is being counted
	gone    bool       // and it has hung up: nothing more is judged
	note    *heldNote
	quit    chan struct{}
	once    sync.Once
}

// startLateWatch: a watch that writes into note. stop ends it.
func startLateWatch(limit time.Duration, note *heldNote) *lateWatch {
	w := &lateWatch{limit: limit, note: note, quit: make(chan struct{})}
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.quit:
				return
			case <-tick.C:
			}
			w.mu.Lock()
			var late *lateMark
			for _, m := range w.owed {
				if w.reading && !w.gone && time.Since(m.at) > w.limit {
					late = &m
				}
			}
			have := w.have
			w.mu.Unlock()
			if late != nil {
				w.note.add(fmt.Sprintf("%d bytes the provider had sent were not with the device %d ms later. The program was here:\n%s",
					late.off-have, time.Since(late.at).Milliseconds(), whereItIs()))
				return
			}
		}
	}()
	return w
}

func (w *lateWatch) stop() { w.once.Do(func() { close(w.quit) }) }

// lateFor: the test's own watch, started the first time it is asked for, with live_rough_test's limit. What it
// wrote down goes into the test's output if the test fails.
func lateFor(t *testing.T) *lateWatch {
	whereMu.Lock()
	defer whereMu.Unlock()
	if w := lateBy[t.Name()]; w != nil {
		return w
	}
	w := startLateWatch(roughLate, &heldNote{})
	lateBy[t.Name()] = w
	t.Cleanup(func() {
		w.stop()
		whereMu.Lock()
		delete(lateBy, t.Name())
		whereMu.Unlock()
		if s := w.note.Load(); s != "" && t.Failed() {
			t.Logf("video was held while this test ran:\n%s", s)
		}
	})
	return w
}

// sent: the provider has sent this many bytes in all by now. sentMore: it has just sent n more.
func (w *lateWatch) sent(all int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.all = all
	w.owed = append(w.owed, lateMark{time.Now(), all})
	w.mu.Unlock()
}

func (w *lateWatch) sentMore(n int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.all += n
	w.owed = append(w.owed, lateMark{time.Now(), w.all})
	w.mu.Unlock()
}

// got: the device has this many bytes in all by now. gotMore: it has just been sent n more.
func (w *lateWatch) got(all int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.have = 0
	w.mu.Unlock()
	w.gotMore(all)
}

func (w *lateWatch) gotMore(n int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.reading, w.have, w.last = true, w.have+n, time.Now()
	// (a packet the provider has sent only part of is not passed on until the rest of it comes)
	for len(w.owed) > 0 && w.owed[0].off-187 <= w.have {
		w.owed = w.owed[1:]
	}
	w.mu.Unlock()
}

// reads: body, with what is read from it counted as what the device has; already: bytes of it read before.
// Only the first device to ask is counted (it is the one sent the provider's bytes as they are; a device that
// joins later starts somewhere else). When it hangs up, nothing more is judged.
func (w *lateWatch) reads(body io.ReadCloser, already int) io.ReadCloser {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reading {
		return body
	}
	w.reading, w.have, w.last = true, already, time.Now()
	return &lateBody{body, w}
}

type lateBody struct {
	io.ReadCloser
	w *lateWatch
}

func (b *lateBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.w.gotMore(n)
	}
	if err != nil {
		b.w.mu.Lock()
		b.w.gone = true
		b.w.mu.Unlock()
	}
	return n, err
}

// newBytesBy: the device must be sent bytes after `after` and before `by`. If none came, where each part of
// the program is at `by` is written down (the waits on the network too: one of them was too long).
func (w *lateWatch) newBytesBy(after, by time.Time, what string) {
	go func() {
		select {
		case <-w.quit:
			return
		case <-time.After(time.Until(by)):
		}
		w.mu.Lock()
		came := w.last.After(after)
		w.mu.Unlock()
		if !came {
			w.note.add(fmt.Sprintf("%s: the device was sent nothing for %.1f s. The program was here:\n%s",
				what, by.Sub(after).Seconds(), whereItIsNow(true)))
		}
	}()
}

// within: something a device waits for must be done in d. If the func returned has not been called by then,
// where each part of the program is at that moment is written down (in the test's lateFor note).
func within(t *testing.T, d time.Duration, what string) (done func()) {
	w := lateFor(t)
	ok := make(chan struct{})
	go func() {
		select {
		case <-ok:
		case <-w.quit:
		case <-time.After(d):
			w.note.add(fmt.Sprintf("%s was not done after %.1f s. The program was here:\n%s", what, d.Seconds(), whereItIsNow(true)))
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(ok) }) }
}

// diffAt: where got first differs from want, for a failure message: the byte, the MPEG-TS packet (188 bytes
// each, the first is 0), 16 bytes of each from there on in hex, and whether it looks like bytes left out or
// bytes added. "" if got is all of want.
func diffAt(want, got []byte) string {
	at := 0
	for at < len(want) && at < len(got) && want[at] == got[at] {
		at++
	}
	if at == len(want) && at == len(got) {
		return ""
	}
	hex := func(b []byte) string {
		if at >= len(b) {
			return "(nothing: it ends here)"
		}
		return fmt.Sprintf("% x", b[at:min(at+16, len(b))])
	}
	s := fmt.Sprintf("the first difference is at byte %d (packet %d, byte %d of that packet)\n    the provider sent: %s\n    the device got:    %s",
		at, at/188, at%188, hex(want), hex(got))
	// Two packets' worth from the difference on, looked for a little further on in the other one.
	const piece, far = 2 * 188, 512 * 188
	if at+piece <= len(got) {
		if i := bytes.Index(want[at:min(len(want), at+far)], got[at:at+piece]); i > 0 {
			s += fmt.Sprintf("\n    what the device got from there on is what the provider sent %d bytes further on: it looks like %d bytes (%.1f packets) were left out", i, i, float64(i)/188)
		}
	}
	if at+piece <= len(want) {
		if i := bytes.Index(got[at:min(len(got), at+far)], want[at:at+piece]); i > 0 {
			s += fmt.Sprintf("\n    what the provider sent from there on reached the device %d bytes further on: it looks like %d bytes (%.1f packets) were added or sent twice", i, i, float64(i)/188)
		}
	}
	return s
}
