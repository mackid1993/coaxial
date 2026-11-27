package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A wait meant as "within N s of the ask" and split between connecting, the answer and the first bytes must pass
// on what is left of N, not N again: a deadline set afresh once the answer came let a connection that answered
// late and then went quiet hold its players up to twice as long as stated (rule 14). Each test here times the
// real worst case against a stand-in that answers late and then goes QUIET (it stays open and sends nothing),
// and each has a partner proving a slow but healthy provider is left alone.

// wbConn: what one provider connection does, timed from its ask. head: it answers then (< 0: never). first: it
// sends its first bytes then (< 0: never; it stays open and quiet). quiet: from then on it sends nothing more
// (0: it plays on). part: it sends this many bytes of the answer, then nothing (a piece that stops part way).
// trickle: the rest of a piece goes out in four parts this many seconds apart. direct: a channel's address is
// answered with its playlist instead of being sent on. packed: a piece is sent with a Content-Encoding (it
// cannot be carried on with a Range).
type wbConn struct {
	head, first, quiet float64
	part               int
	trickle            float64
	direct, packed     bool
}

// wbProv: the stand-in provider. A live MPEG-TS channel 7 (live_lead_test's synthetic feed: an opening burst,
// then lumps 6 s apart) and a live HLS channel 44 sent on by a 302 to its playlist, which lists one piece of
// about 1 MB. What each connection of a path does is planned by its number; past the plan it plays.
type wbProv struct {
	fp    *fakeProv
	start time.Time
	feed  *leadFeed
	later http.HandlerFunc // (the feed as a connection asked for after the first: it starts some seconds back)
	piece []byte

	mu    sync.Mutex
	plans map[string][]wbConn
	asks  map[string][]float64 // when each connection of a path was asked for (seconds from start)
	ends  map[string][]float64 // when the other side let it go (0: still open)
	rngs  map[string][]string  // the Range each asked with
}

const (
	wbTS    = "/live/realU/realP/7.ts"
	wbCh    = "/live/realU/realP/44.m3u8"
	wbList  = "/hlsw/tok44/index.m3u8"
	wbPiece = "/hlsw/tok44/p1.ts"
)

func newWBProv(t *testing.T, plans map[string][]wbConn) *wbProv {
	p := &wbProv{start: time.Now(), feed: syntheticFeed(), plans: plans, asks: map[string][]float64{},
		ends: map[string][]float64{}, rngs: map[string][]string{}}
	p.piece = bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1<<20/188)
	n := &atomic.Int32{}
	n.Store(1) // (leadProvider's own count: past 1, a connection starts back from the newest video)
	p.later = leadProvider(p.feed, p.start, 1e9, n)
	p.fp = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case wbTS, wbCh, wbList, wbPiece:
			p.serve(w, r)
			return true
		}
		return false
	})
	return p
}

func (p *wbProv) wall() float64 { return time.Since(p.start).Seconds() }

// wait: until s seconds after at (seconds from start); false if the other side let go first.
func (p *wbProv) wait(ctx context.Context, at, s float64) bool {
	t := time.NewTimer(time.Until(p.start.Add(secs(at + s))))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *wbProv) asked(path string) []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]float64(nil), p.asks[path]...)
}

func (p *wbProv) ended(path string) []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]float64(nil), p.ends[path]...)
}

func (p *wbProv) serve(w http.ResponseWriter, r *http.Request) {
	path, ctx := r.URL.Path, r.Context()
	p.mu.Lock()
	n := len(p.asks[path])
	at := p.wall()
	p.asks[path], p.ends[path] = append(p.asks[path], at), append(p.ends[path], 0)
	p.rngs[path] = append(p.rngs[path], r.Header.Get("Range"))
	var c wbConn
	if n < len(p.plans[path]) {
		c = p.plans[path][n]
	}
	p.mu.Unlock()
	defer func() {
		if ctx.Err() != nil {
			p.mu.Lock()
			p.ends[path][n] = p.wall()
			p.mu.Unlock()
		}
	}()
	if c.head < 0 || !p.wait(ctx, at, c.head) {
		<-ctx.Done() // (takes the request and never answers)
		return
	}
	switch path {
	case wbTS:
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if c.first < 0 || !p.wait(ctx, at, c.first) {
			<-ctx.Done() // (answered, then quiet)
			return
		}
		var out http.ResponseWriter = w
		if c.quiet > 0 {
			out = &wbQuiet{ResponseWriter: w, ctx: ctx, from: p.start.Add(secs(at + c.quiet))}
		}
		p.later(out, r)
	case wbCh:
		if !c.direct {
			http.Redirect(w, r, p.fp.srv.URL+wbList, 302)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(200)
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		w.(http.Flusher).Flush()
		<-ctx.Done() // (the rest never comes)
	case wbList:
		if c.first < 0 {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-ctx.Done()
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\np1.ts\n")
	case wbPiece:
		from, ranged := 0, false // (a Range is answered 206, bytes=0- too, as servers do)
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from); err == nil {
			ranged = true
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(p.piece)-1, len(p.piece)))
		}
		body := p.piece[from:]
		w.Header().Set("Content-Type", "video/mp2t")
		if c.packed {
			w.Header().Set("Content-Encoding", "x-test")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if ranged {
			w.WriteHeader(206)
		} else {
			w.WriteHeader(200)
		}
		w.(http.Flusher).Flush()
		if c.first < 0 || !p.wait(ctx, at, c.first) {
			<-ctx.Done() // (answered, then quiet)
			return
		}
		if c.part > 0 {
			w.Write(body[:c.part])
			w.(http.Flusher).Flush()
			<-ctx.Done() // (stopped part way)
			return
		}
		if c.trickle > 0 { // (slow but steady: four parts, trickle seconds apart)
			q := len(body) / 4
			for i := 0; i < 4; i++ {
				end := min(len(body), (i+1)*q)
				if i == 3 {
					end = len(body)
				}
				if _, err := w.Write(body[i*q : end]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				if i < 3 && !p.wait(ctx, p.wall(), c.trickle) {
					return
				}
			}
			return
		}
		w.Write(body)
	}
}

// wbQuiet: a connection that sends nothing more from a moment on, and stays open.
type wbQuiet struct {
	http.ResponseWriter
	ctx  context.Context
	from time.Time
}

func (q *wbQuiet) Write(b []byte) (int, error) {
	if time.Now().After(q.from) {
		<-q.ctx.Done()
		return 0, q.ctx.Err()
	}
	return q.ResponseWriter.Write(b)
}

func (q *wbQuiet) Flush() { q.ResponseWriter.(http.Flusher).Flush() }

// wbReader: a device reading a live MPEG-TS answer: its status, when it came, and when bytes came.
type wbReader struct {
	mu       sync.Mutex
	status   int
	answered float64
	at       []float64 // after each read with bytes: when
	n        []int     // and how many bytes in all by then
	done     chan struct{}
}

func wbRead(ctx context.Context, p *wbProv, base, dev, path string) *wbReader {
	rd := &wbReader{done: make(chan struct{})}
	go func() {
		defer close(rd.done)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		req.Header.Set("X-Dev", dev)
		resp, err := devClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		rd.mu.Lock()
		rd.status, rd.answered = resp.StatusCode, p.wall()
		rd.mu.Unlock()
		buf, all := make([]byte, 64<<10), 0
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				all += n
				rd.mu.Lock()
				rd.at, rd.n = append(rd.at, p.wall()), append(rd.n, all)
				rd.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return rd
}

// firstAfter: when the first bytes came at or after wall second t (-1: none did).
func (rd *wbReader) firstAfter(t float64) float64 {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	for _, at := range rd.at {
		if at >= t {
			return at
		}
	}
	return -1
}

// wbGet: one request from device dev, following the proxy's own redirects (a long wait of its own, so the
// proxy's wait is what is timed): the status, the whole answer, the address that gave it, and seconds taken.
func wbGet(base, dev, path string) (int, []byte, string, float64) {
	began := time.Now()
	client := &http.Client{Timeout: 70 * time.Second, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for hop := 0; hop < 6; hop++ {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("X-Dev", dev)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, path, time.Since(began).Seconds()
		}
		if resp.StatusCode == 302 {
			resp.Body.Close()
			loc, _ := url.Parse(resp.Header.Get("Location"))
			b, _ := url.Parse(base + path)
			path = b.ResolveReference(loc).RequestURI()
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return -1, data, path, time.Since(began).Seconds()
		}
		return resp.StatusCode, data, path, time.Since(began).Seconds()
	}
	return 0, nil, path, time.Since(began).Seconds()
}

// ---------------------------------------------------------------------------
// Live MPEG-TS: a channel's first connection (firstWait)
// ---------------------------------------------------------------------------

// The provider answers a channel's first connection late (9 s) and then sends nothing, without closing. Its
// first video must still come within firstWait of the ask, so the new connection goes out by then. It went out
// only after 9 + 12 = 21 s: the first video got firstWait again once the answer came.
func TestWaitFirstOpenAnsweredLateThenQuiet(t *testing.T) {
	p := newWBProv(t, map[string][]wbConn{wbTS: {{head: 9, first: -1}}})
	_, base := limFront(t, p.fp, "ts", "", advanced{})
	ctx, cancel := context.WithTimeout(context.Background(), secs(firstWait+8))
	defer cancel()
	rd := wbRead(ctx, p, base, "192.0.2.71", "/live/tv/easy/7.ts")
	<-rd.done
	asks := p.asked(wbTS)
	if len(asks) < 2 {
		t.Fatalf("the provider was asked once in %d s; a first connection that answered at 9 s and sent nothing "+
			"must be asked again within firstWait (%d s) of its ask", firstWait+8, firstWait)
	}
	if d := asks[1] - asks[0]; d > firstWait+1.5 {
		t.Errorf("the new connection went out %.1f s after the first ask, want within %d s (+1 s for an answer "+
			"in the last second)", d, firstWait)
	}
	if at := rd.firstAfter(0); rd.status != 200 || at < 0 || at > firstWait+3 {
		t.Errorf("the player was answered %d, first video at %.1f s", rd.status, at)
	}
}

// A slow but healthy first connection: it answers at 6 s and sends its opening burst at 10.5 s, both within
// firstWait of the ask. It is asked once and plays.
func TestWaitFirstOpenSlowAnswerLeftAlone(t *testing.T) {
	p := newWBProv(t, map[string][]wbConn{wbTS: {{head: 6, first: 10.5}}})
	_, base := limFront(t, p.fp, "ts", "", advanced{})
	ctx, cancel := context.WithTimeout(context.Background(), secs(firstWait+6))
	defer cancel()
	rd := wbRead(ctx, p, base, "192.0.2.73", "/live/tv/easy/7.ts")
	<-rd.done
	if n := len(p.asked(wbTS)); n != 1 {
		t.Errorf("the provider was asked %d times, want 1: a slow but healthy first answer was given up on", n)
	}
	if at := rd.firstAfter(0); rd.status != 200 || at < 10 || at > 12 {
		t.Errorf("the player was answered %d, first video at %.1f s (the provider sent it at 10.5 s)", rd.status, at)
	}
}

// A provider that takes longer than firstWait only to answer, every time: each connection is let go after
// firstWait, it is asked again twice (three asks in all, firstWait apart), and then the player is answered
// instead of being kept waiting for ever. No fourth ask follows.
func TestWaitFirstOpenSlowerThanFirstWaitAskedTwiceMore(t *testing.T) {
	const slow = firstWait + 1.0
	p := newWBProv(t, map[string][]wbConn{wbTS: {{head: slow, first: slow}, {head: slow, first: slow}, {head: slow, first: slow}, {head: slow, first: slow}}})
	_, base := limFront(t, p.fp, "ts", "", advanced{})
	ctx, cancel := context.WithTimeout(context.Background(), secs(3*firstWait+6))
	defer cancel()
	rd := wbRead(ctx, p, base, "192.0.2.74", "/live/tv/easy/7.ts")
	<-rd.done
	asks, ends := p.asked(wbTS), p.ended(wbTS)
	if len(asks) != 3 {
		t.Fatalf("the provider was asked %d times (at %v), want 3: the first ask and two more", len(asks), asks)
	}
	for i := range asks {
		if took := ends[i] - asks[i]; ends[i] == 0 || took > firstWait+1 || took < firstWait-1 {
			t.Errorf("connection %d was let go %.1f s after its ask (0: never), want about %d s", i+1, took, firstWait)
		}
	}
	rd.mu.Lock()
	st, answered, got := rd.status, rd.answered, len(rd.at)
	rd.mu.Unlock()
	if st == 200 && got > 0 || st == 0 || answered > 3*firstWait+3 {
		t.Errorf("the player was answered %d at %.1f s with %d reads of video; want a refusal by %d s",
			st, answered, got, 3*firstWait+3)
	}
}
