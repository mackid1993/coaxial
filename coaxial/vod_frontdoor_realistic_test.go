package main

// Films, episodes and catch-up through the front door, against a provider that behaves the way real ones do:
// films and catch-up answered as files (200, or 206 for a Range), an episode sent on to a second server with
// the account's real login in the address, several seconds before the first byte, a connection that goes quiet
// part way without closing (through a VPN tunnel that happens), an account that allows a small number of
// connections and ends the oldest when one more comes, and catch-up that the provider counts as its live
// channel (newest wins). Players open their new read before closing the old one when they seek, and some
// look at a film with HEAD before they play it.
//
// Each rule is tested both ways: it acts when it should, and it stays out of the way of healthy use.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const fdrSize = 3<<20 + 41

// fdrPlan: what one ask of a path does at the stand-in provider.
type fdrPlan struct {
	start      time.Duration // before the answer's headers (a server slow to start)
	pauseAfter int           // after this many bytes, a pause of pause (0: none)
	pause      time.Duration
	quietAfter int // after this many bytes, nothing more and no close, until the reader hangs up (0: never)
}

// fdrRead: one read open at the provider. unit: what the provider counts as one connection (reads of one
// catch-up file at once are one); channel: the channel it is, for a provider that allows one read per channel.
type fdrRead struct {
	unit, channel string
	at            time.Time
	kick          chan struct{}
}

type fdrProv struct {
	own, video *fakeProv
	film       []byte

	mu               sync.Mutex
	cap              int           // connections the account allows at once (0: any number)
	catchUpIsChannel bool          // catch-up of a channel counts as that channel
	pace             time.Duration // between 16 KB sends (0: as fast as it is read)
	plans            map[string][]fdrPlan
	types            map[string]string // Content-Type by path ("-": none sent)
	asks             map[string]int    // by path, GET and HEAD
	heads            map[string]int
	open             map[*fdrRead]bool
	kicked           int // reads the provider ended because one more came (someone's stream cut)
	reads            int
}

func newFdrProv(t *testing.T) *fdrProv {
	p := &fdrProv{film: make([]byte, fdrSize), plans: map[string][]fdrPlan{}, types: map[string]string{},
		asks: map[string]int{}, heads: map[string]int{}, open: map[*fdrRead]bool{}}
	x := uint64(0x9e3779b97f4a7c15)
	for i := range p.film { // (no two stretches alike: bytes from the wrong place show)
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		p.film[i] = byte(x >> 24)
	}
	p.video = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		// the second server: the episode, at an address with the account's real login and a token
		if !strings.HasPrefix(r.URL.Path, "/series/realU/realP/") || !strings.HasPrefix(r.URL.Query().Get("token"), "t") {
			w.WriteHeader(403)
			return true
		}
		p.file(w, r, "", "")
		return true
	})
	var tok int
	p.own = newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		switch {
		case path == "/series/realU/realP/9.mp4":
			p.mu.Lock()
			tok++
			n := tok
			p.mu.Unlock()
			w.Header().Set("Location", p.video.srv.URL+"/series/realU/realP/9.mp4?token=t"+strconv.Itoa(n))
			w.WriteHeader(302)
		case strings.HasSuffix(path, "/404.mkv"):
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			io.WriteString(w, "<html><body>404 Not Found</body></html>\n")
		case strings.HasPrefix(path, "/movie/realU/realP/") || strings.HasPrefix(path, "/series/realU/realP/"):
			p.file(w, r, "", "")
		case strings.HasPrefix(path, "/timeshift/realU/realP/"):
			ch := "cu" + channelID(path)
			if p.count(func(p *fdrProv) int { return map[bool]int{true: 1}[p.catchUpIsChannel] }) == 1 {
				ch = channelID(path)
			}
			p.file(w, r, "file "+path, ch)
		case strings.HasPrefix(path, "/live/realU/realP/") && strings.HasSuffix(path, ".ts"):
			p.live(w, r, channelID(path))
		default:
			return false
		}
		return true
	})
	return p
}

func (p *fdrProv) set(f func(p *fdrProv)) { p.mu.Lock(); f(p); p.mu.Unlock() }

func (p *fdrProv) count(f func(p *fdrProv) int) int { p.mu.Lock(); defer p.mu.Unlock(); return f(p) }

// enter: a read arrives. The account's rules apply as a provider applies them: a read that would go over the
// connections allowed, or a second read of a channel, ends the oldest one in the way. A read Coaxial already let
// go of is given a moment to be seen closing first (the stand-in's own delay in noticing a hang-up). unit "":
// a read of its own.
func (p *fdrProv) enter(unit, channel string) *fdrRead {
	p.mu.Lock()
	p.reads++
	if unit == "" {
		unit = "read " + strconv.Itoa(p.reads)
	}
	p.mu.Unlock()
	rd := &fdrRead{unit: unit, channel: channel, at: time.Now(), kick: make(chan struct{})}
	for end := time.Now().Add(300 * time.Millisecond); ; time.Sleep(5 * time.Millisecond) {
		p.mu.Lock()
		joins, units := false, map[string]*fdrRead{}
		var clash []*fdrRead
		for o := range p.open {
			switch {
			case o.unit == unit:
				joins = true
			case channel != "" && o.channel == channel:
				clash = append(clash, o)
			}
			if u := units[o.unit]; u == nil || o.at.Before(u.at) {
				units[o.unit] = o
			}
		}
		over := !joins && p.cap > 0 && len(units) >= p.cap
		if joins || (!over && len(clash) == 0) || time.Now().After(end) {
			if !joins {
				for _, o := range clash {
					p.end(o)
				}
				for over && len(units) >= p.cap { // the oldest goes
					var old *fdrRead
					for _, o := range units {
						if old == nil || o.at.Before(old.at) {
							old = o
						}
					}
					delete(units, old.unit)
					for o := range p.open {
						if o.unit == old.unit {
							p.end(o)
						}
					}
				}
			}
			p.open[rd] = true
			p.mu.Unlock()
			return rd
		}
		p.mu.Unlock()
	}
}

func (p *fdrProv) end(o *fdrRead) { // (mu held)
	delete(p.open, o)
	close(o.kick)
	p.kicked++
}

func (p *fdrProv) leave(rd *fdrRead) {
	p.mu.Lock()
	delete(p.open, rd)
	p.mu.Unlock()
}

func (p *fdrProv) opened() int { return p.count(func(p *fdrProv) int { return len(p.open) }) }

// wait: false when the reader hung up or the read was ended by the provider meanwhile.
func fdrWait(r *http.Request, rd *fdrRead, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	var kick chan struct{}
	if rd != nil {
		kick = rd.kick
	}
	select {
	case <-r.Context().Done():
		return false
	case <-kick:
		return false
	case <-time.After(d):
		return true
	}
}

// file: the film (or the catch-up file) answered as a file server answers, by the plan for this ask.
func (p *fdrProv) file(w http.ResponseWriter, r *http.Request, unit, channel string) {
	p.mu.Lock()
	n := p.asks[r.URL.Path]
	p.asks[r.URL.Path]++
	if r.Method == "HEAD" {
		p.heads[r.URL.Path]++
	}
	var pl fdrPlan
	if list := p.plans[r.URL.Path]; n < len(list) {
		pl = list[n]
	}
	kind, pace := p.types[r.URL.Path], p.pace
	p.mu.Unlock()
	var rd *fdrRead
	if r.Method == "GET" {
		rd = p.enter(unit, channel)
		defer p.leave(rd)
	}
	if !fdrWait(r, rd, pl.start) {
		return
	}
	size, from, to, status := len(p.film), 0, len(p.film)-1, 200
	h := w.Header()
	switch kind {
	case "":
		h.Set("Content-Type", "video/mp4")
	case "-":
		h["Content-Type"] = nil // (none sent, and none made up)
	default:
		h.Set("Content-Type", kind)
	}
	h.Set("Accept-Ranges", "bytes")
	if v, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
		a, b, _ := strings.Cut(v, "-")
		switch {
		case a == "":
			k, _ := strconv.Atoi(b)
			from = max(0, size-k)
		case b == "":
			from, _ = strconv.Atoi(a)
		default:
			from, _ = strconv.Atoi(a)
			to, _ = strconv.Atoi(b)
			to = min(to, size-1)
		}
		status = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
	}
	h.Set("Content-Length", strconv.Itoa(to-from+1))
	w.WriteHeader(status)
	if r.Method == "HEAD" {
		return
	}
	w.(http.Flusher).Flush()
	paused := false
	for at, sent := from, 0; at <= to; {
		if pl.quietAfter > 0 && sent >= pl.quietAfter {
			fdrWait(r, rd, time.Hour) // quiet, and never closed by the server itself
			return
		}
		if pl.pauseAfter > 0 && sent >= pl.pauseAfter && !paused {
			paused = true
			if !fdrWait(r, rd, pl.pause) {
				return
			}
		}
		k := min(to+1, at+16<<10) - at
		if _, err := w.Write(p.film[at : at+k]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		at, sent = at+k, sent+k
		if !fdrWait(r, rd, pace) {
			return
		}
	}
}

// live: a live channel as MPEG-TS, until the reader hangs up or the provider ends it.
func (p *fdrProv) live(w http.ResponseWriter, r *http.Request, id string) {
	p.mu.Lock()
	p.asks[r.URL.Path]++
	p.mu.Unlock()
	rd := p.enter("", id)
	defer p.leave(rd)
	chunk := make([]byte, 188*100)
	for i := 0; i < len(chunk); i += 188 {
		chunk[i] = 0x47
	}
	w.Header().Set("Content-Type", "video/mp2t")
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if !fdrWait(r, rd, 10*time.Millisecond) {
			return
		}
	}
}

// fdrReading: a player's read, its answer read in the background as it arrives.
type fdrReading struct {
	resp *http.Response
	took time.Duration // to the answer's headers
	mu   sync.Mutex
	got  []byte
	err  error
	done chan struct{}
}

// fdrAsk: device dev asks for path (with a Range when rg is not ""), as a player does: on a connection of its
// own, not following redirects (the test follows them as the player would).
func fdrAsk(t *testing.T, method, base, dev, path, rg string) *fdrReading {
	t.Helper()
	x, err := fdrTry(method, base, dev, path, rg)
	if err != nil {
		t.Fatalf("%s %s %s: %v", dev, path, rg, err)
	}
	t.Cleanup(func() { x.resp.Body.Close() })
	return x
}

// fdrTry: fdrAsk for a goroutine of the test's own (the caller closes the answer).
func fdrTry(method, base, dev, path, rg string) (*fdrReading, error) {
	req, _ := http.NewRequest(method, base+path, nil)
	req.Header.Set("X-Dev", dev)
	if rg != "" {
		req.Header.Set("Range", rg)
	}
	began := time.Now()
	resp, err := devClient.Do(req)
	if err != nil {
		return nil, err
	}
	x := &fdrReading{resp: resp, took: time.Since(began), done: make(chan struct{})}
	go func() {
		defer close(x.done)
		buf := make([]byte, 64<<10)
		for {
			n, err := resp.Body.Read(buf)
			x.mu.Lock()
			x.got = append(x.got, buf[:n]...)
			if err != nil {
				if err != io.EOF {
					x.err = err
				}
				x.mu.Unlock()
				return
			}
			x.mu.Unlock()
		}
	}()
	return x, nil
}

func (x *fdrReading) n() int { x.mu.Lock(); defer x.mu.Unlock(); return len(x.got) }

// ended: whether the answer is over within d, and whether it broke off (err) or ended as complete.
func (x *fdrReading) ended(d time.Duration) (bool, error) {
	select {
	case <-x.done:
		x.mu.Lock()
		defer x.mu.Unlock()
		return true, x.err
	case <-time.After(d):
		return false, nil
	}
}

func (x *fdrReading) bytes() []byte {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]byte(nil), x.got...)
}

// fdrSame: the reading got exactly the film from byte from to its end, and ended as complete.
func fdrSame(t *testing.T, what string, x *fdrReading, film []byte, from int, d time.Duration) {
	t.Helper()
	over, err := x.ended(d)
	if !over {
		t.Fatalf("%s: the answer did not end within %.0f s (%d bytes so far)", what, d.Seconds(), x.n())
	}
	if got := x.bytes(); err != nil || !bytes.Equal(got, film[from:]) {
		t.Errorf("%s: %d bytes (%v); want the film's %d bytes from byte %d on, exactly", what, len(got), err, len(film)-from, from)
	}
}

// A film whose server takes 5 s to send anything (an overloaded film server, or one that builds the file
// first). A player straight at the provider waits for it; through Coaxial it is waited for too, asked for once
// and passed on whole. (A wait on a film's first byte cut down to a live piece's would answer it 504.)
func TestFrontdoorFilmSlowStartIsWaitedFor(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) { p.plans["/movie/realU/realP/31.mkv"] = []fdrPlan{{start: 5 * time.Second}} })
	_, base := limFront(t, p.own, "ts", "1", advanced{})
	x := fdrAsk(t, "GET", base, "192.0.2.11", "/movie/tv/easy/31.mkv", "bytes=0-")
	if x.resp.StatusCode != 206 || x.resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-%d/%d", fdrSize-1, fdrSize) {
		t.Fatalf("a film slow to start: %d, Content-Range %q, want 206 for the whole film", x.resp.StatusCode, x.resp.Header.Get("Content-Range"))
	}
	if x.took < 4500*time.Millisecond {
		t.Errorf("answered after %.1f s; the stand-in does not answer before 5 s", x.took.Seconds())
	}
	fdrSame(t, "a film slow to start", x, p.film, 0, 10*time.Second)
	if n := p.count(func(p *fdrProv) int { return p.asks["/movie/realU/realP/31.mkv"] }); n != 1 {
		t.Errorf("the provider was asked for the film %d times, want once", n)
	}
}

// A film whose server sends the first megabyte, then nothing for 10 s, then the rest (a busy film server; a
// tunnel that stood still a moment). A film is not a live piece: the player decides how long it waits, and a
// player that waits gets the rest from its one read. Through Coaxial the same, with the server asked once.
func TestFrontdoorFilmPauseMidwayCarriesOn(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) {
		p.plans["/series/realU/realP/32.mp4"] = []fdrPlan{{pauseAfter: 1 << 20, pause: 10 * time.Second}}
	})
	_, base := limFront(t, p.own, "ts", "", advanced{})
	x := fdrAsk(t, "GET", base, "192.0.2.12", "/series/tv/easy/32.mp4", "")
	if x.resp.StatusCode != 200 {
		t.Fatalf("an episode: %d", x.resp.StatusCode)
	}
	fdrSame(t, "an episode whose server paused 10 s part way", x, p.film, 0, 20*time.Second)
	if n := p.count(func(p *fdrProv) int { return p.asks["/series/realU/realP/32.mp4"] }); n != 1 {
		t.Errorf("the provider was asked for the episode %d times, want once", n)
	}
}

// An account with one connection. The film's connection goes quiet part way without closing (the tunnel
// lost it). The player gives up on its own and asks for the rest with a Range, opening the new read before it
// lets go of the old one. The new read must start at once (not after the grace a closing read gets), with the
// bytes from where the player got to; the quiet read must be let go at the provider first, so the provider
// never has to end one of the two; and the old answer must end broken, never as a short whole one.
func TestFrontdoorFilmQuietReadMakesRoomForTheRetry(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) {
		p.cap, p.pace = 1, 10*time.Millisecond
		p.plans["/movie/realU/realP/33.mkv"] = []fdrPlan{{quietAfter: 512 << 10}}
	})
	_, base := limFront(t, p.own, "ts", "1", advanced{})
	first := fdrAsk(t, "GET", base, "192.0.2.13", "/movie/tv/easy/33.mkv", "bytes=0-")
	if first.resp.StatusCode != 206 {
		t.Fatalf("the first read: %d", first.resp.StatusCode)
	}
	for end := time.Now().Add(5 * time.Second); first.n() < 512<<10 && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Second) // (the player's own read timeout, scaled down)
	got := first.n()
	if got != 512<<10 {
		t.Fatalf("the first read has %d bytes before it went quiet, want %d", got, 512<<10)
	}
	retry := fdrAsk(t, "GET", base, "192.0.2.13", "/movie/tv/easy/33.mkv", fmt.Sprintf("bytes=%d-", got))
	if retry.resp.StatusCode != 206 || retry.resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", got, fdrSize-1, fdrSize) {
		t.Fatalf("the retry: %d, Content-Range %q", retry.resp.StatusCode, retry.resp.Header.Get("Content-Range"))
	}
	if retry.took > time.Second {
		t.Errorf("the retry was answered after %.1f s: the quiet read it replaces held it up", retry.took.Seconds())
	}
	if over, err := first.ended(2 * time.Second); !over || err == nil {
		t.Errorf("the quiet read was replaced: its answer must end broken (ended %v, error %v)", over, err)
	}
	fdrSame(t, "the retry", retry, p.film, got, 20*time.Second)
	if k := p.count(func(p *fdrProv) int { return p.kicked }); k != 0 {
		t.Errorf("the provider had to end %d read(s): the quiet one was still open at the provider when its retry came", k)
	}
}

// An episode the provider sends on to its second server, with the account's real login in that address, on
// an account with one connection. The player is slow to follow (1.5 s: an app that starts its player first).
// Meanwhile another device asks for a film and is turned away (the place is the episode's), and the provider
// is not asked for it. The player then follows and plays, and seeks by opening its new read before closing
// the old one: the seek starts at once with the right bytes. The player is never handed the real login, the
// second server is always asked with it, and no read is ever ended by the provider. Once the player stops,
// the other device gets in at once.
func TestFrontdoorEpisodeOnSecondServerKeepsItsPlace(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) { p.cap, p.pace = 1, 10*time.Millisecond })
	_, base := limFront(t, p.own, "ts", "1", advanced{})
	r := ask(t, base, "192.0.2.21", "/series/tv/easy/9.mp4")
	loc := r.Header.Get("Location")
	r.Body.Close()
	if r.StatusCode != 302 || !strings.HasPrefix(loc, "/__cdn/http/") || strings.Contains(loc, "realU") || strings.Contains(loc, "realP") ||
		!strings.Contains(loc, "/tv/easy/") {
		t.Fatalf("the episode's redirect: %d %q, want one back to this proxy with the easy login", r.StatusCode, loc)
	}
	time.Sleep(1500 * time.Millisecond)
	other := ask(t, base, "192.0.2.22", "/movie/tv/easy/40.mkv")
	if text := body(other); other.StatusCode != 503 {
		t.Errorf("another device while the episode's player follows its redirect: %d %s, want 503", other.StatusCode, fdrShort(text))
	}
	if n := p.count(func(p *fdrProv) int { return p.asks["/movie/realU/realP/40.mkv"] }); n != 0 {
		t.Errorf("the provider was asked for the refused film")
	}
	first := fdrAsk(t, "GET", base, "192.0.2.21", loc, "bytes=0-")
	if first.resp.StatusCode != 206 {
		t.Fatalf("the episode at the address it was sent to: %d %q", first.resp.StatusCode, body(first.resp))
	}
	time.Sleep(300 * time.Millisecond)
	const seekAt = 2000000
	seek := fdrAsk(t, "GET", base, "192.0.2.21", loc, fmt.Sprintf("bytes=%d-", seekAt))
	if seek.resp.StatusCode != 206 || seek.resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", seekAt, fdrSize-1, fdrSize) {
		t.Fatalf("the seek: %d, Content-Range %q", seek.resp.StatusCode, seek.resp.Header.Get("Content-Range"))
	}
	if seek.took > time.Second {
		t.Errorf("the seek took %.1f s to start", seek.took.Seconds())
	}
	if over, err := first.ended(2 * time.Second); !over || err == nil {
		t.Errorf("the read the seek replaced: ended %v, %d bytes, error %v; want it cut, and broken", over, first.n(), err)
	}
	fdrSame(t, "the seek", seek, p.film, seekAt, 20*time.Second)
	if k := p.count(func(p *fdrProv) int { return p.kicked }); k != 0 {
		t.Errorf("the provider had to end %d read(s)", k)
	}
	if n := p.video.count(p.video.hits, "/series/realU/realP/9.mp4"); n != 2 {
		t.Errorf("the second server was asked %d times with the real login, want 2 (the read and the seek)", n)
	}
	eventually(t, func() bool { return p.opened() == 0 }, "the episode's reads closing")
	late := fdrAsk(t, "GET", base, "192.0.2.22", "/movie/tv/easy/40.mkv", "")
	if late.resp.StatusCode != 200 || late.took > time.Second {
		t.Errorf("the other device once the episode stopped: %d after %.1f s, want 200 at once", late.resp.StatusCode, late.took.Seconds())
	}
}

// An account with one connection, a film playing on one device. Another device looks at a film with HEAD
// (its size and type, before it plays): that is no stream, so it is passed on and answered, and the film
// playing is not cut. The device playing looks at its own film with HEAD: its read goes on too.
func TestFrontdoorHeadIsNoStream(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) { p.cap, p.pace = 1, 10*time.Millisecond })
	_, base := limFront(t, p.own, "ts", "1", advanced{})
	film := fdrAsk(t, "GET", base, "192.0.2.31", "/movie/tv/easy/60.mkv", "")
	if film.resp.StatusCode != 200 {
		t.Fatalf("the film: %d", film.resp.StatusCode)
	}
	for _, c := range []struct{ dev, path string }{{"192.0.2.32", "/movie/tv/easy/61.mkv"}, {"192.0.2.31", "/movie/tv/easy/60.mkv"}} {
		h := fdrAsk(t, "HEAD", base, c.dev, c.path, "")
		if h.resp.StatusCode != 200 || h.resp.Header.Get("Content-Length") != strconv.Itoa(fdrSize) {
			t.Errorf("HEAD %s from %s: %d, Content-Length %q; want the provider's 200 with the film's length", c.path, c.dev,
				h.resp.StatusCode, h.resp.Header.Get("Content-Length"))
		}
	}
	if n := p.count(func(p *fdrProv) int {
		return p.heads["/movie/realU/realP/61.mkv"] + p.heads["/movie/realU/realP/60.mkv"]
	}); n != 2 {
		t.Errorf("the provider was asked %d HEADs, want 2", n)
	}
	if over, _ := film.ended(500 * time.Millisecond); over {
		t.Errorf("the film playing ended after a HEAD (%d of %d bytes)", film.n(), fdrSize)
	}
	fdrSame(t, "the film playing", film, p.film, 0, 20*time.Second)
}

// A provider that locks the account to one address, a film asked for 2 s before the wait after the VPN came
// up is over (the player tried as soon as the network was back). A few seconds are waited out rather than
// refused: the player is answered once the wait is over, and the provider is asked once. (Far from the end of
// the wait it is refused, with Retry-After: TestLockWait.)
func TestFrontdoorFilmAskedJustBeforeTheAddressWaitEnds(t *testing.T) {
	p := newFdrProv(t)
	_, base := advFront(t, p.own, "ts", advanced{StayUnder: off(), Locked: true})
	mu.Lock()
	was := vpnChanged
	vpnChanged = mono() - (defaultLockWait - 2)
	mu.Unlock()
	defer func() { mu.Lock(); vpnChanged = was; mu.Unlock() }()
	x := fdrAsk(t, "GET", base, "192.0.2.41", "/movie/tv/easy/70.mkv", "")
	if x.resp.StatusCode != 200 {
		t.Fatalf("a film 2 s before the wait is over: %d %q, want it waited out", x.resp.StatusCode, body(x.resp))
	}
	if x.took < 1500*time.Millisecond || x.took > 5*time.Second {
		t.Errorf("answered after %.1f s, want about 2 (when the wait is over)", x.took.Seconds())
	}
	fdrSame(t, "the film", x, p.film, 0, 10*time.Second)
	if n := p.count(func(p *fdrProv) int { return p.asks["/movie/realU/realP/70.mkv"] }); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
}

// fdrShort: an answer's text for a failure message (a film answered by mistake is shown by its length).
func fdrShort(text string) string {
	if len(text) > 200 || !utf8.ValidString(text) {
		return fmt.Sprintf("(%d bytes)", len(text))
	}
	return strconv.Quote(text)
}

// fdrPlays: whether device who counts as playing (what the launcher asks before a VPN restart or a server
// move, and the status list before it asks a provider anything).
func fdrPlays(who string) bool {
	for _, p := range playingNow() {
		if p.(map[string]any)["device"] == who {
			return true
		}
	}
	return false
}

// A film whose server sends no Content-Type, and a catch-up whose server types its .ts file as text (a web
// server left at its defaults maps .ts to a translation file type). Both are being watched: they must count as
// playing while they are read, so that no VPN restart, update or server move comes in under them (AGENTS.md
// rules 1 and 10). A film the provider answers 404 is not being watched.
func TestFrontdoorUntypedFilmCountsAsPlaying(t *testing.T) {
	p := newFdrProv(t)
	p.set(func(p *fdrProv) {
		p.pace = 10 * time.Millisecond
		p.types["/series/realU/realP/50.mp4"] = "-"
		p.types["/timeshift/realU/realP/60/2026-01-01:10-00/13.ts"] = "text/vnd.trolltech.linguist"
	})
	devs := []string{"192.0.2.51", "192.0.2.52", "192.0.2.53"}
	defer func() {
		mu.Lock()
		for _, d := range devs {
			delete(films, d)
		}
		mu.Unlock()
	}()
	_, base := limFront(t, p.own, "ts", "", advanced{})
	for i, path := range []string{"/series/tv/easy/50.mp4", "/timeshift/tv/easy/60/2026-01-01:10-00/13.ts"} {
		x := fdrAsk(t, "GET", base, devs[i], path, "")
		if x.resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, x.resp.StatusCode)
		}
		for end := time.Now().Add(3 * time.Second); x.n() == 0 && time.Now().Before(end); {
			time.Sleep(20 * time.Millisecond)
		}
		if !fdrPlays(devs[i]) {
			t.Errorf("%s is being read (%d bytes so far, Content-Type %q) and does not count as playing", path, x.n(), x.resp.Header.Get("Content-Type"))
		}
	}
	x := fdrAsk(t, "GET", base, devs[2], "/movie/tv/easy/404.mkv", "")
	if over, _ := x.ended(3 * time.Second); x.resp.StatusCode != 404 || !over {
		t.Fatalf("a film the provider does not have: %d", x.resp.StatusCode)
	}
	if fdrPlays(devs[2]) {
		t.Errorf("a film the provider answered 404 counts as playing")
	}
}
