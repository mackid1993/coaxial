package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A stand-in provider that behaves the way real providers were seen to, for the *_realistic_test.go files.
//
// Live MPEG-TS: an opening burst of several seconds of video, then lumps 6 to 12 s apart with pauses of up to
// about 17 s at peak load. Each lump goes out as several writes of uneven sizes, so a read at the other end
// ends in the middle of a packet. Every channel address is first answered with a 302 to a streaming address on
// the same server, as providers do. A channel has one session: a new connection for it ends the one before.
// An account has a connection cap: a new connection past it ends the oldest one. A connection opened later
// starts some seconds back from the newest video (its own opening burst). What a given connection of a channel
// does (go quiet without closing, drop in the middle of a packet, answer 404 while the channel restarts, take
// the request and never answer) is set per connection number.
//
// Live HLS: a playlist that states one target duration and moves on at its own pace (one provider states 4 s
// and moves every 6.2 to 6.7 s, up to 8 s), sent on to a second server by a 302, pieces sent in uneven writes
// with a Content-Length.
//
// The wall-clock times in a plan are multiplied by scale. The program's own waits are constants, so a test that
// meets one of them runs at scale 1; a test that does not can run faster.

// rpFeed: a live channel's video. About 2 Mbit/s of 50 fps H.264-style video on PID 0x100 (the PCR on every
// frame, a keyframe marked every 1.5 s with the program tables just before it) and sound on PID 0x101.
type rpFeed struct {
	data    []byte
	frameAt []int // where frame i starts (a keyframe: at the program tables before it)
	fps     int
}

const rpGOP = 1.5 // seconds between keyframes

// rpBuild: seconds of video whose clock starts at clock (seconds); per: packets of video per frame.
func rpBuild(seconds int, clock float64, per int) *rpFeed {
	const fps = 50
	gop := int(rpGOP * fps)
	pat := leadTable(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00})
	pmt := leadTable(0x1000, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0,
		0x1b, 0xe1, 0x00, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0})
	var vcc, acc byte
	pkt := func(pid int, cc *byte, start bool, af []byte, pts int64, tag byte) []byte {
		p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10 | *cc&0x0f}
		*cc++
		if start {
			p[1] |= 0x40
		}
		if af != nil {
			p[3] |= 0x20
			p = append(append(p, byte(len(af))), af...)
		}
		if start {
			sid := byte(0xe0)
			if pid == 0x101 {
				sid = 0xc0
			}
			p = append(p, 0, 0, 1, sid, 0, 0, 0x80, 0x80, 5,
				0x21|byte(pts>>29)&0x0e, byte(pts>>22), byte(pts>>14)|1, byte(pts>>7), byte(pts<<1)|1)
		}
		for len(p) < 188 {
			p = append(p, tag)
		}
		return p
	}
	f := &rpFeed{fps: fps}
	for i := 0; i < seconds*fps; i++ {
		f.frameAt = append(f.frameAt, len(f.data))
		key := i%gop == 0
		if key {
			f.data = append(append(f.data, pat...), pmt...)
		}
		at := clock + float64(i)/fps
		b := int64(at * 90000)
		af := []byte{0x10, byte(b >> 25), byte(b >> 17), byte(b >> 9), byte(b >> 1), byte(b&1)<<7 | 0x7e, 0}
		if key {
			af[0] |= 0x40
		}
		pts := b + 45000
		f.data = append(f.data, pkt(0x100, &vcc, true, af, pts, byte(i)|1)...)
		for k := 1; k < per; k++ {
			f.data = append(f.data, pkt(0x100, &vcc, false, nil, 0, byte(i)|1)...)
			if k%12 == 0 {
				f.data = append(f.data, pkt(0x101, &acc, k == 12, nil, pts, 0x5a)...)
			}
		}
	}
	return f
}

// upTo: where second s of video starts (the whole feed past its end).
func (f *rpFeed) upTo(s float64) int {
	i := int(s*float64(f.fps) + 0.5)
	if i >= len(f.frameAt) {
		return len(f.data)
	}
	return f.frameAt[max(i, 0)]
}

// rpLump: at wall second at, the provider has sent the channel's video up to second upTo.
type rpLump struct{ at, upTo float64 }

// rpLumps: an opening burst of burst seconds of video (in two reads 1.5 s apart, as seen), then one lump at each
// of the wall times given, each carrying the video up to that time plus the burst.
func rpLumps(burst float64, at ...float64) []rpLump {
	out := []rpLump{{0, burst - 3}, {1.5, burst}}
	for _, t := range at {
		out = append(out, rpLump{t, t + burst})
	}
	return out
}

// rpConn: what one connection of a channel does (the zero value: plays as planned).
type rpConn struct {
	status  int     // answered with this status and a short text instead of video (a channel restarting: 404)
	noHead  bool    // takes the request and never answers it
	noBytes bool    // answers 200 and then sends nothing, without closing
	quietAt float64 // from the first lump at or after this wall second on, it sends nothing and stays open
	dropAt  float64 // in the first lump at or after this wall second it sends half, cut in a packet, and closes
}

// rpChannel: one channel of the provider.
type rpChannel struct {
	feed  *rpFeed
	lumps []rpLump
	back  float64  // a connection opened after the first starts this many seconds of video behind the newest
	conns []rpConn // by connection number from 1 (index 0); past the end: plays as planned
}

type rpOpen struct {
	id    string
	n     int
	since time.Time
	end   context.CancelFunc
}

// tsRealProv: the stand-in provider.
type tsRealProv struct {
	fp    *fakeProv
	start time.Time
	scale float64
	cap   int // connections the account may hold at once (0: no cap)

	mu     sync.Mutex
	chans  map[string]*rpChannel
	asks   map[string]int // connections asked for (after the redirect), by channel
	open   []*rpOpen      // connections open now, oldest first
	most   int            // the most open at once
	kicked map[string]int // connections ended to make room (the cap) or by a new one for the same channel
	sent   map[string]int // what the first connection of a channel has sent, in bytes
	began  map[string]int // where in its feed the first connection of a channel started (see firstFrom)
	pieces map[string]*rpHLS
}

func newTSRealProv(t *testing.T, scale float64, cap int) *tsRealProv {
	p := &tsRealProv{scale: scale, cap: cap, chans: map[string]*rpChannel{}, asks: map[string]int{},
		kicked: map[string]int{}, sent: map[string]int{}, pieces: map[string]*rpHLS{}}
	p.fp = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool { return p.serve(w, r) })
	p.began = map[string]int{}
	p.start = time.Now()
	return p
}

// wall: seconds of the plan since the provider started.
func (p *tsRealProv) wall() float64 { return time.Since(p.start).Seconds() / p.scale }

func (p *tsRealProv) until(ctx context.Context, at float64) bool {
	t := time.NewTimer(time.Until(p.start.Add(time.Duration(at * p.scale * float64(time.Second)))))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *tsRealProv) add(id string, c *rpChannel) { p.mu.Lock(); p.chans[id] = c; p.mu.Unlock() }

func (p *tsRealProv) asked(id string) int { p.mu.Lock(); defer p.mu.Unlock(); return p.asks[id] }

func (p *tsRealProv) serve(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/live/realU/realP/") { // the channel's address: sent on to the streaming address
		id, ext, _ := strings.Cut(strings.TrimPrefix(path, "/live/realU/realP/"), ".")
		p.mu.Lock()
		_, ts := p.chans[id]
		_, hls := p.pieces[id]
		p.mu.Unlock()
		if ext == "ts" && ts {
			http.Redirect(w, r, fmt.Sprintf("/play/tok%s/%s.ts", id, id), 302)
			return true
		}
		if ext == "m3u8" && hls {
			http.Redirect(w, r, fmt.Sprintf("%s/cdn/%s/index.m3u8", p.fp.srv.URL, id), 302)
			return true
		}
		return false
	}
	if strings.HasPrefix(path, "/play/") && strings.HasSuffix(path, ".ts") {
		p.stream(w, r, channelID(path))
		return true
	}
	if strings.HasPrefix(path, "/cdn/") {
		p.serveHLS(w, r)
		return true
	}
	return false
}

// stream: one connection of a live MPEG-TS channel.
func (p *tsRealProv) stream(w http.ResponseWriter, r *http.Request, id string) {
	p.mu.Lock()
	c := p.chans[id]
	if c == nil {
		p.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	p.asks[id]++
	n := p.asks[id]
	plan := rpConn{}
	if n-1 < len(c.conns) {
		plan = c.conns[n-1]
	}
	if plan.status != 0 {
		p.mu.Unlock()
		http.Error(w, "channel not available", plan.status)
		return
	}
	ctx, end := context.WithCancel(r.Context())
	defer end()
	me := &rpOpen{id: id, n: n, since: time.Now(), end: end}
	var keep []*rpOpen
	for _, o := range p.open { // one session per channel: the new connection ends the one before
		if o.id == id {
			o.end()
			p.kicked[id]++
		} else {
			keep = append(keep, o)
		}
	}
	p.open = keep
	if p.cap > 0 && len(p.open) >= p.cap { // the account's cap: the oldest connection is ended
		p.open[0].end()
		p.kicked[p.open[0].id]++
		p.open = p.open[1:]
	}
	p.open = append(p.open, me)
	p.most = max(p.most, len(p.open))
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		for i, o := range p.open {
			if o == me {
				p.open = append(p.open[:i], p.open[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
	}()
	if plan.noHead {
		<-ctx.Done()
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(200)
	w.(http.Flusher).Flush()
	if plan.noBytes {
		<-ctx.Done()
		return
	}
	k, from := 0, 0
	if n > 1 || p.wall() > 0.5 { // opened later: its opening burst starts some seconds back from the newest video
		now := p.wall()
		for k < len(c.lumps) && c.lumps[k].at <= now {
			k++
		}
		if k > 0 {
			newest := c.lumps[k-1].upTo
			from = c.feed.upTo(newest - c.back)
			if !p.send(ctx, w, id, n, c.feed.data[from:c.feed.upTo(newest)], from) {
				return
			}
			from = c.feed.upTo(newest)
		}
	}
	if n == 1 {
		p.mu.Lock()
		if _, ok := p.began[id]; !ok {
			p.began[id] = from
			if k > 0 {
				p.began[id] = c.feed.upTo(c.lumps[k-1].upTo - c.back)
			}
		}
		p.mu.Unlock()
	}
	for ; k < len(c.lumps); k++ {
		l := c.lumps[k]
		if !p.until(ctx, l.at) {
			return
		}
		to := c.feed.upTo(l.upTo)
		if plan.quietAt > 0 && l.at >= plan.quietAt {
			<-ctx.Done() // quiet: open, and nothing more
			return
		}
		if plan.dropAt > 0 && l.at >= plan.dropAt { // half the lump, cut off inside a packet, then closed
			p.send(ctx, w, id, n, c.feed.data[from:from+(to-from)/2/188*188+94], from)
			panic(http.ErrAbortHandler)
		}
		if !p.send(ctx, w, id, n, c.feed.data[from:to], from) {
			return
		}
		from = to
	}
	<-ctx.Done()
}

// rpWrites: the uneven sizes a lump goes out in (none a whole number of packets).
var rpWrites = []int{61*188 + 97, 33*188 + 5, 120*188 + 150, 7*188 + 1, 250*188 + 111}

func (p *tsRealProv) send(ctx context.Context, w http.ResponseWriter, id string, n int, b []byte, off int) bool {
	for i := 0; len(b) > 0; i++ {
		m := min(len(b), rpWrites[i%len(rpWrites)])
		if _, err := w.Write(b[:m]); err != nil {
			return false
		}
		w.(http.Flusher).Flush()
		b, off = b[m:], off+m
		if n == 1 {
			p.mu.Lock()
			p.sent[id] = off
			p.mu.Unlock()
		}
		if i%4 == 3 { // (the writes of one lump arrive close together, at about 280 Mbit/s, but not as one read)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Live HLS
// ---------------------------------------------------------------------------

// rpHLS: a live HLS channel. Piece i holds the video from cut[i] to cut[i+1] (seconds) and appears in the
// playlist at wall second shown[i] (negative: before the test began); the playlist lists the newest six.
type rpHLS struct {
	feed   *rpFeed
	target int // the stated #EXT-X-TARGETDURATION
	cut    []float64
	shown  []float64
	asks   map[string]int // requests by path
}

// addHLS: a live HLS channel whose pieces are each one of lens (seconds of video, whole keyframe intervals),
// in turn, the playlist moving on as each piece's own length goes by; six pieces are listed at the start.
func (p *tsRealProv) addHLS(id string, feed *rpFeed, target int, lens ...float64) *rpHLS {
	h := &rpHLS{feed: feed, target: target, asks: map[string]int{}}
	at, i := 0.0, 0
	for ; at < float64(len(feed.frameAt))/float64(feed.fps)-10; i++ {
		h.cut = append(h.cut, at)
		at += lens[i%len(lens)]
	}
	h.cut = append(h.cut, at)
	for k := range h.cut[:len(h.cut)-1] { // piece 5 is the newest at the start
		h.shown = append(h.shown, h.cut[k+1]-h.cut[6])
	}
	p.mu.Lock()
	p.pieces[id] = h
	p.mu.Unlock()
	return h
}

func (h *rpHLS) piece(i int) []byte {
	return h.feed.data[h.feed.upTo(h.cut[i]):h.feed.upTo(h.cut[i+1])]
}

// newest: the newest piece listed at wall second now.
func (h *rpHLS) newest(now float64) int {
	k := 0
	for k+1 < len(h.shown) && h.shown[k+1] <= now {
		k++
	}
	return k
}

func (p *tsRealProv) serveHLS(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/") // /cdn/<id>/<name>
	if len(parts) != 4 {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	h := p.pieces[parts[2]]
	if h != nil {
		h.asks[parts[3]]++
	}
	p.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	if parts[3] == "index.m3u8" {
		top := h.newest(p.wall())
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", h.target, top-5)
		for i := top - 5; i <= top; i++ {
			fmt.Fprintf(&b, "#EXTINF:%.3f,\np%d.ts\n", h.cut[i+1]-h.cut[i], i)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, b.String())
		return
	}
	var i int
	if _, err := fmt.Sscanf(parts[3], "p%d.ts", &i); err != nil || i < 0 || i+1 >= len(h.cut) || h.shown[i] > p.wall() {
		http.NotFound(w, r)
		return
	}
	data := h.piece(i)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	for k := 0; len(data) > 0; k++ {
		m := min(len(data), rpWrites[k%len(rpWrites)])
		if _, err := w.Write(data[:m]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		data = data[m:]
		if k%4 == 3 {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

// rpFront: the front door for the stand-in provider, set to mode ("ts" or "hls") with this many connections
// typed in ("": none). Devices name their address with X-Dev.
func rpFront(t *testing.T, p *tsRealProv, mode string, connections number) string {
	_, base := limFront(t, p.fp, mode, connections, advanced{})
	return base
}

// rpReader: a device reading a live MPEG-TS answer: every byte and when it came.
type rpReader struct {
	mu     sync.Mutex
	status int
	data   []byte
	t      []float64 // after each read: when (wall seconds of the provider's plan), and how much was in data
	n      []int
	ended  float64 // when the answer ended (0: still open when the test stopped it)
	done   chan struct{}
}

// rpRead: device dev asks for path and reads all of it until ctx ends. pace > 0: after its first
// startSecs seconds it reads only pace bytes a second (a player with a small buffer, reading as it plays).
func rpRead(ctx context.Context, p *tsRealProv, base, dev, path string, pace, startSecs float64) *rpReader {
	rd := &rpReader{done: make(chan struct{})}
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
		rd.status = resp.StatusCode
		rd.mu.Unlock()
		began, base := time.Now(), -1
		buf := make([]byte, 64<<10)
		for {
			if pace > 0 {
				if el := time.Since(began).Seconds(); el > startSecs {
					rd.mu.Lock()
					if base < 0 {
						base = len(rd.data)
					}
					owe := float64(len(rd.data)) - (float64(base) + (el-startSecs)*pace)
					rd.mu.Unlock()
					if owe > 0 {
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Duration(owe / pace * float64(time.Second))):
						}
					}
				}
			}
			n, err := resp.Body.Read(buf)
			rd.mu.Lock()
			rd.data = append(rd.data, buf[:n]...)
			if n > 0 {
				rd.t, rd.n = append(rd.t, p.wall()), append(rd.n, len(rd.data))
			}
			if err != nil && ctx.Err() == nil {
				rd.ended = p.wall()
			}
			rd.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return rd
}

// view: what the reader was sent, as a player counts it (see leadView): seconds of video in hand after each
// read, times the clock went backwards, and the joins where video was left out.
func (rd *rpReader) view(asked float64) *leadView {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	v := &leadView{asked: asked}
	pcrPID, last, have, started := -1, 0.0, 0.0, false
	i := 0
	for k, end := range rd.n {
		for ; i+188 <= end; i += 188 {
			if rd.data[i] != 0x47 { // (out of step: find the next packet start)
				for i < end && rd.data[i] != 0x47 {
					i++
				}
				i -= 188
				continue
			}
			p := rd.data[i : i+188]
			pid := int(p[1]&0x1f)<<8 | int(p[2])
			if p[3]&0x20 == 0 || p[4] == 0 {
				continue
			}
			rai, hasPCR := p[5]&0x40 != 0, p[4] >= 7 && p[5]&0x10 != 0
			if !hasPCR || (pcrPID >= 0 && pid != pcrPID) {
				continue
			}
			pcrPID = pid
			pcr := float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7) / 90000
			switch d := pcr - last; {
			case !started:
				started = rai
			case d < 0:
				v.back++
			case d < 1:
				have += d
			default:
				v.skips, v.skipAtKey = append(v.skips, d), append(v.skipAtKey, rai)
			}
			if started {
				last = pcr
			}
		}
		v.t, v.have = append(v.t, rd.t[k]), append(v.have, have)
	}
	return v
}

func (rd *rpReader) bytes() []byte {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return append([]byte(nil), rd.data...)
}

// rpGet: one request from device dev, following the proxy's own redirects: the status, the whole answer and
// the address that gave it.
func rpGet(t *testing.T, base, dev, path string) (int, []byte, string) {
	t.Helper()
	for hop := 0; hop < 6; hop++ {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("X-Dev", dev)
		resp, err := devClient.Do(req)
		if err != nil {
			return 0, nil, path
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
			return -1, data, path
		}
		return resp.StatusCode, data, path
	}
	return 0, nil, path
}

// rpList: the pieces a playlist lists (their addresses), its MEDIA-SEQUENCE and DISCONTINUITY-SEQUENCE, and the
// numbers of the pieces marked as not following on (#EXT-X-DISCONTINUITY).
func rpList(list []byte) (pieces []string, seq, disc int, ended bool, discAt []int) {
	seq, disc = -1, -1
	mark := false
	for _, l := range strings.Split(string(list), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			fmt.Sscan(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), &seq)
		case strings.HasPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			fmt.Sscan(strings.TrimPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"), &disc)
		case l == "#EXT-X-DISCONTINUITY":
			mark = true
		case l == "#EXT-X-ENDLIST":
			ended = true
		case l != "" && !strings.HasPrefix(l, "#"):
			if mark {
				discAt = append(discAt, seq+len(pieces))
			}
			pieces, mark = append(pieces, l), false
		}
	}
	return
}

// rpPlayer: an HLS player on device dev: it asks for the channel, reloads the playlist every reload seconds
// and fetches each new piece it lists once, until ctx ends.
type rpPlayer struct {
	mu        sync.Mutex
	firstList float64 // seconds from asking to the first playlist
	lists     int
	seqs      []int // MEDIA-SEQUENCE of each playlist
	discs     []int
	discAt    map[int]bool // pieces marked as not following on, by number
	ended     bool
	fails     []string // what went wrong, in words
	got       map[string]int
	data      [][]byte  // the pieces, in the order fetched
	pieceAt   []float64 // and when each came
}

func rpPlay(ctx context.Context, t *testing.T, p *tsRealProv, base, dev, channel string, reload float64) *rpPlayer {
	pl := &rpPlayer{firstList: -1, got: map[string]int{}, discAt: map[int]bool{}}
	go func() {
		asked := p.wall()
		at := channel
		for ctx.Err() == nil {
			st, list, from := rpGet(t, base, dev, at)
			if ctx.Err() != nil {
				return
			}
			pl.mu.Lock()
			if st != 200 {
				pl.fails = append(pl.fails, fmt.Sprintf("playlist answered %d at %.1f s", st, p.wall()))
				pl.mu.Unlock()
			} else {
				if pl.firstList < 0 {
					pl.firstList = p.wall() - asked
				}
				pieces, seq, disc, ended, discAt := rpList(list)
				for _, n := range discAt {
					pl.discAt[n] = true
				}
				pl.lists++
				pl.seqs, pl.discs = append(pl.seqs, seq), append(pl.discs, disc)
				pl.ended = pl.ended || ended
				pl.mu.Unlock()
				at = from // (its next reloads go to the address it was sent on to)
				for _, piece := range pieces {
					pl.mu.Lock()
					had := pl.got[piece] > 0
					pl.mu.Unlock()
					if had {
						continue
					}
					u, _ := url.Parse(base + at)
					ref, _ := url.Parse(piece)
					pst, data, _ := rpGet(t, base, dev, u.ResolveReference(ref).RequestURI())
					pl.mu.Lock()
					if pst != 200 {
						pl.fails = append(pl.fails, fmt.Sprintf("piece %s answered %d at %.1f s", piece, pst, p.wall()))
					} else {
						pl.got[piece]++
						pl.data, pl.pieceAt = append(pl.data, data), append(pl.pieceAt, p.wall())
					}
					pl.mu.Unlock()
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(reload * p.scale * float64(time.Second))):
			}
		}
	}()
	return pl
}

// rpIsPart: where got is found whole in want (-1: it is not), for a reader that should have been passed the
// provider's bytes untouched.
// firstFrom: where in its feed the first connection of channel id started. On a slow machine the first ask can
// come later than 0.5 s after the provider started, and then its opening burst starts some seconds back from the
// newest video, as a real provider's does.
func (p *tsRealProv) firstFrom(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.began[id]
}

func rpIsPart(want, got []byte) int {
	if len(got) == 0 {
		return -1
	}
	head := got[:min(len(got), 4096)]
	for from := 0; ; {
		i := bytes.Index(want[from:], head)
		if i < 0 {
			return -1
		}
		if bytes.HasPrefix(want[from+i:], got) {
			return from + i
		}
		from += i + 1
	}
}
