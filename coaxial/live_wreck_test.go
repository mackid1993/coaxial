package main

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The live MPEG-TS path, fed the worst things real providers do, and held to what a viewer experiences.
// live_rough_test.go sends packets with no programme in them down one connection that never ends. Here the
// channel is a real one (program tables, H.264 video with keyframes, sound, PCR, PTS and DTS), so the code
// that keeps the clock running on is exercised too, and the provider behaves as the incidents in AGENTS.md
// say providers do:
//
//   - it ends the connection in the middle of a lump, once and again, and its new connection starts some
//     seconds back; it answers 404 or 503 for a while after a drop; its new connection sends nothing at first;
//   - its clock starts over near zero in the middle of a connection (a 24/7 channel's next episode), leaps
//     ahead, or wraps after 26 hours;
//   - twelve devices come and go on the channel around one that stays;
//   - feeds made from a seed: lump sizes, pauses, where reads end, when and how the provider drops.
//
// What is measured is what the viewer gets: the provider's bytes, in order, wherever the design passes them
// through; how late each byte is; whether the clock it is sent ever steps back or leaps; seconds of video in
// hand at a player; how long video stops against how long the provider itself stopped.
//
// What the design gives, and these tests hold it to no more than that:
//   - a watched channel whose connection ends after reopenAfter seconds or more is asked for again at once;
//     while the provider answers 404 or a server error it is asked again after 2, 3, 5, 10, 10, 15 and 15 s
//     (reaskBackoff), about a minute in all; a stream that ran under reopenAfter is not asked for again;
//   - a stream asked for again is joined on at its first keyframe past the clock already passed on, on the
//     188-byte grid (a packet the drop cut short is filled out);
//   - a joiner is handed at once about joinSeconds of video, from a keyframe on;
//   - after a clock jump only whole packets are passed on, and a few frames are held until it is known
//     whether the provider is sending video again (replay.go);
//   - a read that ends inside the first bytes of the very packet the clock jumps in lets that one packet
//     through on the provider's clock (see track in main.go, and TestClockFixNoWorse). The feeds here keep
//     reads from ending there, so that everything else can be held to an exact rule.
//
// The scenes run side by side (the drops need reopenAfter seconds each before anything happens).
// COAXIAL_ROUGH_SEED=<n> sends the seeded feeds of an earlier run again; COAXIAL_ROUGH_LONG=1 adds more and
// longer ones.

const (
	wreckLate = 750 * time.Millisecond // how long after the provider sent a byte a device watching may be sent it
	wreckFPS  = 25
	wreckGOP  = 50 // frames from one keyframe to the next: 2 s
)

func wreckLong() bool { v := os.Getenv("COAXIAL_ROUGH_LONG"); return v != "" && v != "0" }

// ---------------------------------------------------------------------------
// The channel's stream
// ---------------------------------------------------------------------------

// wreckPart: a stretch of the channel on one clock.
type wreckPart struct {
	secs  int     // how long it plays (a whole number of keyframe intervals)
	clock float64 // where its clock (PCR) starts, seconds
	file  bool    // a new file: its first packets carry the discontinuity flag and its counters start over
}

// wreckStream: a channel as an encoder makes it. H.264 video on PID 0x100 (25 frames a second, a keyframe
// every 2 s with the program tables just before it, the PCR on every frame, PTS and DTS half a second past it,
// pictures shown in another order than sent) and AAC sound on PID 0x101. Every frame's bytes are its own.
type wreckStream struct {
	data   []byte
	frames []int  // where each video frame starts (a keyframe: at the tables before it)
	key    []bool // whether it is a keyframe
	parts  []int  // the first frame of each part
}

func wreckBuild(seed uint64, parts ...wreckPart) *wreckStream {
	s := &wreckStream{}
	cc := map[int]byte{}
	x := seed*0x9e3779b97f4a7c15 + 1
	pkt := func(pid int, start bool, af, payload []byte) {
		p := make([]byte, 0, 188)
		p = append(p, 0x47, byte(pid>>8), byte(pid), 0x10|cc[pid]&0x0f)
		cc[pid]++
		if start {
			p[1] |= 0x40
		}
		if af != nil {
			p[3] |= 0x20
			p = append(append(p, byte(len(af))), af...)
		}
		p = append(p, payload...)
		for len(p) < 188 { // (never a zero: no start code hides in it)
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			p = append(p, 0x10|byte(x>>24))
		}
		s.data = append(s.data, p[:188]...)
	}
	ts := func(prefix byte, v int64) []byte {
		v &= tsWrap - 1
		return []byte{prefix<<4 | byte(v>>29)&0x0e | 1, byte(v >> 22), byte(v>>14)&0xfe | 1, byte(v >> 7), byte(v<<1)&0xfe | 1}
	}
	pat := leadTable(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0x00})
	pmt := leadTable(0x1000, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0,
		0x1b, 0xe1, 0x00, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0})
	tables := func(disc bool) {
		for _, tb := range [][]byte{pat, pmt} {
			p := append([]byte(nil), tb...)
			pid := int(p[1]&0x1f)<<8 | int(p[2])
			p[3] = 0x10 | cc[pid]&0x0f
			cc[pid]++
			if disc { // (the flag needs an adaptation field: a 2-byte one before the section)
				p = append([]byte{p[0], p[1], p[2], p[3] | 0x20, 1, 0x80}, p[4:]...)[:188]
			}
			s.data = append(s.data, p...)
		}
	}
	keyES := []byte{0, 0, 0, 1, 0x09, 0x10, 0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0, 0, 0, 1, 0x68, 0xee, 0x3c, 0x80, 0, 0, 0, 1, 0x65, 0x88}
	otherES := []byte{0, 0, 0, 1, 0x09, 0x30, 0, 0, 0, 1, 0x41, 0x9a}
	for _, part := range parts {
		s.parts = append(s.parts, len(s.frames))
		if part.file {
			for pid := range cc {
				cc[pid] = 0
			}
		}
		pcr0 := int64(part.clock * 90000)
		aPTS := pcr0 + 45000
		for f := 0; f < part.secs*wreckFPS; f++ {
			isKey, first := f%wreckGOP == 0, part.file && f == 0
			pcr := (pcr0 + int64(f)*90000/wreckFPS) & (tsWrap - 1)
			s.frames, s.key = append(s.frames, len(s.data)), append(s.key, isKey)
			size, es, flags := 30, otherES, byte(0)
			if isKey {
				tables(first)
				size, es, flags = 150, keyES, 0x40
			}
			if first {
				flags |= 0x80
			}
			dts := pcr0 + int64(f)*90000/wreckFPS + 45000
			pts := dts + int64([]int{3, 0, 0}[f%3])*90000/wreckFPS
			pes := append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10}, append(ts(3, pts), ts(1, dts)...)...)
			pkt(0x100, true, []byte{0x10 | flags, byte(pcr >> 25), byte(pcr >> 17), byte(pcr >> 9), byte(pcr >> 1), byte(pcr&1)<<7 | 0x7e, 0}, append(pes, es...))
			for i := 1; i < size; i++ {
				pkt(0x100, false, nil, nil)
			}
			for ; aPTS <= dts+90000/wreckFPS; aPTS += 1920 { // (1024 samples at 48 kHz)
				var af []byte
				room := 184 - 6
				if first {
					af, first, room = []byte{0x80}, false, room-2
				}
				pkt(0x101, true, af, append([]byte{0, 0, 1, 0xc0, byte(room >> 8), byte(room), 0x80, 0x80, 5}, append(ts(2, aPTS), 0xff, 0xf1, 0x4c, 0x80)...))
			}
		}
	}
	return s
}

// frameOf: the last frame that starts at or before pos.
func (s *wreckStream) frameOf(pos int) int {
	return max(0, sort.SearchInts(s.frames, pos+1)-1)
}

// nextKey: the first keyframe at or after frame f (the number of frames when there is none).
func (s *wreckStream) nextKey(f int) int {
	for f = max(f, 0); f < len(s.frames) && !s.key[f]; f++ {
	}
	return f
}

// stamped: where frame f's first video packet is, the one with its PCR, PTS and DTS.
func (s *wreckStream) stamped(f int) int {
	if s.key[f] {
		return s.frames[f] + 2*188
	}
	return s.frames[f]
}

// wreckShared: 104 s of a channel whose clock runs on, for every scene that needs no other.
var wreckShared = sync.OnceValue(func() *wreckStream { return wreckBuild(1, wreckPart{secs: 104, clock: 1000}) })

// wreckFeed: the stream as a provider sends it: piece k (it ends at end[k]) goes out at[k] seconds after the
// start, on whichever connection is open then.
type wreckFeed struct {
	*wreckStream
	at  []float64
	end []int
}

// wreckLumps: when a provider sends: an opening burst, more 2 s later, then a lump every 6 s, one of them
// after a 12 s pause; until the given second. (A device that watches must stay a second or two past the last
// lump, to be sent all of it.)
func wreckLumps(until float64) []float64 {
	out := []float64{0, 2, 8, 14, 20}
	for t := 32.0; t <= until; t += 6 {
		out = append(out, t)
	}
	return out
}

// wreckLive: a live channel. The opening burst is burst seconds of video; every later lump brings the channel
// up to lead seconds past the time it is sent at, in 64 KB writes. A place in apart, when given, is sent in a
// write of its own a moment after the one before it (see the last rule at the top of this file).
func wreckLive(s *wreckStream, burst, lead float64, lumps []float64, apart ...int) *wreckFeed {
	fd := &wreckFeed{wreckStream: s}
	from := 0
	for i, at := range lumps {
		c := at + lead
		if i == 0 {
			c = burst
		}
		if f := int(c * wreckFPS); f < len(s.frames) {
			from = fd.lump(from, s.frames[f], at, apart)
		}
	}
	return fd
}

func (fd *wreckFeed) lump(from, to int, at float64, apart []int) int {
	for from < to {
		e := min(to, from+64<<10)
		for _, q := range apart {
			if lo, hi := q-8<<10, q+16<<10; from < lo && lo < e {
				e = lo
			} else if from == lo {
				e, at = min(to, hi), at+0.06
			}
		}
		fd.at, fd.end = append(fd.at, at), append(fd.end, e)
		from = e
	}
	return from
}

// wreckPlanned: the stream sent as a plan says (wait, then so many bytes in one write), whatever is in it.
func wreckPlanned(s *wreckStream, plan []roughStep) *wreckFeed {
	fd := &wreckFeed{wreckStream: s}
	at, end := 0.0, 0
	for _, st := range plan {
		at += st.wait.Seconds()
		if n := min(len(s.data)-end, st.n); n > 0 {
			end += n
			fd.at, fd.end = append(fd.at, at), append(fd.end, end)
		}
	}
	return fd
}

// ---------------------------------------------------------------------------
// The provider
// ---------------------------------------------------------------------------

// wreckDrop: the provider ends the connection.
type wreckDrop struct {
	at    float64 // seconds after the start
	clean bool    // between lumps, on a whole packet (else: part way into the lump that comes then)
	cut   int     // bytes of that lump sent before the cut (0: half its first write, in the middle of a packet)
}

type wreckSend struct {
	at       time.Time
	conn     int // which ask it was sent on, from 1
	from, to int // the stream's bytes
}

// wreckProv: a provider with one live channel. A connection opened after a drop starts, as providers' do,
// back seconds of video behind the newest video sent, in the middle of a frame.
type wreckProv struct {
	fd        *wreckFeed
	start     time.Time
	back      float64
	drops     []wreckDrop
	refuse    int     // after a drop it answers this status...
	refuseFor float64 // ...for this many seconds
	quietEach float64 // after a drop, every new connection sends nothing for its first seconds
	quietAll  float64 // after a drop, the channel sends nothing for this many seconds, whoever asks

	mu       sync.Mutex
	asks     []float64 // when each ask came
	dropped  []float64 // when each drop was made
	sends    []wreckSend
	once     sync.Once
	finished chan struct{} // closed once everything was sent
}

func (p *wreckProv) serve(w http.ResponseWriter, r *http.Request) {
	fd := p.fd
	now := func() float64 { return time.Since(p.start).Seconds() }
	p.mu.Lock()
	p.asks = append(p.asks, now())
	conn, last := len(p.asks), -1.0
	if n := len(p.dropped); n > 0 {
		last = p.dropped[n-1]
	}
	p.mu.Unlock()
	if last >= 0 && p.refuse != 0 && now() < last+p.refuseFor {
		http.Error(w, "stream unavailable", p.refuse)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(200)
	w.(http.Flusher).Flush()
	until := func(s float64) bool {
		select {
		case <-r.Context().Done():
			return false
		case <-time.After(time.Until(p.start.Add(secs(s)))):
			return true
		}
	}
	send := func(from, to int) bool {
		_, err := w.Write(fd.data[from:to])
		w.(http.Flusher).Flush()
		if err != nil {
			return false
		}
		p.mu.Lock()
		p.sends = append(p.sends, wreckSend{time.Now(), conn, from, to})
		p.mu.Unlock()
		return true
	}
	drop := func() {
		p.mu.Lock()
		p.dropped = append(p.dropped, now())
		p.mu.Unlock()
	}
	from, k := 0, 0
	if last >= 0 {
		if !until(math.Max(last+p.quietAll, now()+p.quietEach)) {
			return
		}
		for t := now(); k < len(fd.at) && fd.at[k] <= t; k++ {
		}
		if k > 0 {
			e := fd.end[k-1]
			if f := fd.frameOf(e-1) - int(p.back*wreckFPS); f > 0 {
				from = fd.frames[f] + 188
			}
			if !send(from, e) {
				return
			}
			from = e
		}
	}
	for ; k < len(fd.at); k++ {
		var d *wreckDrop
		p.mu.Lock()
		if n := len(p.dropped); n < len(p.drops) {
			d = &p.drops[n]
		}
		p.mu.Unlock()
		if d != nil && d.clean && d.at <= fd.at[k] {
			if until(d.at) {
				drop()
			}
			return
		}
		if !until(fd.at[k]) {
			return
		}
		if d != nil && !d.clean && fd.at[k] >= d.at && (k == 0 || fd.at[k]-fd.at[k-1] >= 1) {
			n := d.cut
			if n <= 0 || n >= fd.end[k]-from {
				n = (fd.end[k]-from)/2/188*188 + 94
			}
			send(from, from+n)
			drop()
			panic(http.ErrAbortHandler)
		}
		if !send(from, fd.end[k]) {
			return
		}
		from = fd.end[k]
	}
	p.once.Do(func() { close(p.finished) })
	<-r.Context().Done()
}

// wreckFront: a front door on the provider.
func wreckFront(t *testing.T, p *wreckProv) string {
	p.finished = make(chan struct{})
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") {
			return false
		}
		p.serve(w, r)
		return true
	})
	_, base := limFront(t, fp, "ts", "", advanced{})
	return base
}

// ---------------------------------------------------------------------------
// A device
// ---------------------------------------------------------------------------

type wreckMark struct {
	at  time.Time
	off int // bytes read by then, in all
}

// wreckView: what one device was sent, and when.
type wreckView struct {
	asked, ended time.Time
	status       int
	marks        []wreckMark
	read         []byte
	done         chan struct{}
	hangUp       func()
}

var wreckClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// wreckWatch: device dev asks for the channel and reads all it is sent, until stop or the stream's end.
func wreckWatch(base, dev string) (*wreckView, error) {
	v := &wreckView{asked: time.Now(), done: make(chan struct{})}
	req, _ := http.NewRequest("GET", base+"/live/tv/easy/7.ts", nil)
	req.Header.Set("X-Dev", dev)
	resp, err := wreckClient.Do(req)
	if err != nil {
		return nil, err
	}
	v.status, v.hangUp = resp.StatusCode, func() { resp.Body.Close() }
	go func() {
		defer close(v.done)
		buf := make([]byte, 256<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				v.read = append(v.read, buf[:n]...)
				v.marks = append(v.marks, wreckMark{time.Now(), len(v.read)})
			}
			if err != nil {
				v.ended = time.Now()
				return
			}
		}
	}()
	return v, nil
}

func (v *wreckView) stop() { v.hangUp(); <-v.done }

// player: what the device was sent as a player takes it (see watch in live_lead_test.go): seconds of video
// from its first keyframe on after each read, times the clock went back, and steps ahead of a second or more.
func (v *wreckView) player(start time.Time) *leadView {
	lv := &leadView{asked: v.asked.Sub(start).Seconds()}
	last, have, started, m := 0.0, 0.0, false, 0
	flush := func(upTo int) {
		for ; m < len(v.marks) && v.marks[m].off < upTo; m++ {
			lv.t, lv.have = append(lv.t, v.marks[m].at.Sub(start).Seconds()), append(lv.have, have)
		}
	}
	for i := 0; i+188 <= len(v.read); i += 188 {
		flush(i + 188)
		p := v.read[i : i+188]
		if p[0] != 0x47 || p[1]&0x1f != 0x01 || p[2] != 0 || p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 {
			continue
		}
		rai := p[5]&0x40 != 0
		pcr := float64(int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7) / 90000
		switch d := pcr - last; {
		case !started:
			started = rai
		case d < 0:
			lv.back++
		case d < 1:
			have += d
		default:
			lv.skips, lv.skipAtKey = append(lv.skips, d), append(lv.skipAtKey, rai)
		}
		if started {
			last = pcr
		}
	}
	flush(len(v.read) + 1)
	return lv
}

// ---------------------------------------------------------------------------
// What was sent against what arrived
// ---------------------------------------------------------------------------

// wreckDiffer: the first byte at which what a device was sent differs from the provider's bytes (want, which
// start at a packet); -1: none, as far as the shorter of the two goes. stamps: timestamp fields are not compared.
func wreckDiffer(got, want []byte, stamps bool) int {
	n := min(len(got), len(want))
	a, b := got[:n], want[:n]
	if stamps {
		a, b = append([]byte(nil), a...), append([]byte(nil), b...)
		for _, s := range ecStamps(b) {
			if s.field+5 <= n {
				copy(a[s.field:s.field+5], make([]byte, 5))
				copy(b[s.field:s.field+5], make([]byte, 5))
			}
		}
	}
	if bytes.Equal(a, b) {
		return -1
	}
	k := 0
	for k < n && a[k] == b[k] {
		k++
	}
	return k
}

// wreckSeg: a run of the provider's bytes as the device was sent them.
type wreckSeg struct {
	conn     int // the provider connection they came on
	from, to int // which of the stream's bytes
	at       int // where they are in what the device read
}

// wreckFollow holds what a device that watched from the start was sent against what the provider sent,
// connection by connection. The first connection's bytes come through as they are. Each later one (the
// channel was asked for again) is joined on at one of its keyframes: the first one past the video already
// sent, or the first one it has, if that is later. A packet a drop cut short is filled out with 0xff first
// (fills: where those packets are), or, once the clock has jumped, was never passed on. From the join on,
// again, every byte of it, in order. ok=false: an error was reported and the rest could not be followed.
func wreckFollow(t *testing.T, s *wreckStream, sends []wreckSend, read []byte, stamps bool) (segs []wreckSeg, fills []int, ok bool) {
	t.Helper()
	type conn struct{ id, from, to int }
	var conns []conn
	for _, x := range sends {
		if n := len(conns); n > 0 && conns[n-1].id == x.conn {
			conns[n-1].to = x.to
		} else {
			conns = append(conns, conn{x.conn, x.from, x.to})
		}
	}
	sec := func(f int) float64 { return float64(f) / wreckFPS }
	pos := 0
	for i, c := range conns {
		start := c.from
		if i > 0 {
			if r := pos % 188; r != 0 {
				n := 188 - r
				if pos+n > len(read) || bytes.Count(read[pos:pos+n], []byte{0xff}) != n {
					t.Errorf("drop %d cut a packet short after %d of its bytes; it was not filled out to a whole packet before the stream carried on", i, r)
					return
				}
				fills, pos = append(fills, pos-r), pos+n
			}
			first := s.frameOf(c.from)
			if s.frames[first] < c.from {
				first++
			}
			first = s.nextKey(first)
			got := -1
			for f := first; f < len(s.frames) && s.frames[f] < c.to; f = s.nextKey(f + 1) {
				w := s.data[s.frames[f]:min(c.to, s.frames[f]+6*188)]
				if pos+len(w) <= len(read) && wreckDiffer(read[pos:pos+len(w)], w, stamps) < 0 {
					got = f
					break
				}
			}
			if got < 0 {
				t.Errorf("after drop %d the device was not sent the provider's new stream from one of its keyframes on (%d bytes had gone to the device, %d followed)", i, pos, len(read)-pos)
				return
			}
			start = s.frames[got]
			prev := conns[i-1].to
			cut := s.frameOf(prev - 1) // the last frame whose clock went out whole on the old connection
			for cut >= 0 && s.stamped(cut)+188 > prev {
				cut--
			}
			want := max(s.nextKey(cut+1), first)
			if got != want && !(cut >= 0 && s.key[cut] && got == max(cut, first)) {
				t.Errorf("after drop %d the stream was joined on at the keyframe at %.2f s of the channel; the device had video up to %.2f s and the new connection started at %.2f s, so it should carry on at the keyframe at %.2f s",
					i, sec(got), sec(cut), sec(s.frameOf(c.from)), sec(want))
			}
		}
		want, have, last := s.data[start:c.to], read[pos:], i == len(conns)-1
		n := len(want)
		switch d := wreckDiffer(have, want, stamps); {
		case d < 0 && len(have) >= len(want):
		case d < 0 && last && len(have) >= len(want)-187: // (the start of a packet whose rest never came)
			n = len(have)
		case d < 0:
			t.Errorf("connection %d sent %d bytes from where the device was joined on; the device was sent %d of them", c.id, len(want), len(have))
			return
		default:
			// (after a clock jump only whole packets are passed on: the start of the packet a drop cut short is not)
			if e := len(want) - (pos+len(want))%188; !last && e >= 0 && d >= e {
				n = e
				break
			}
			t.Errorf("the device was not sent what the provider sent: the first difference is at byte %d of the stream (packet %d), byte %d of what the device read",
				start+d, (start+d)/188, pos+d)
			return
		}
		if last && len(have) > len(want) {
			t.Errorf("the device was sent %d bytes more than the provider sent", len(have)-len(want))
		}
		segs = append(segs, wreckSeg{conn: c.id, from: start, to: start + n, at: pos})
		pos += n
	}
	return segs, fills, true
}

// wreckRuns: which of the stream's video frames a device was sent, in the order it was sent them, as runs
// ({0, 1203}, {1250, 1700}: frames 0 to 1203, then 1250 to 1700; -1: a frame that is not the stream's), and
// where in what it read each frame starts (the last time it was sent).
func wreckRuns(s *wreckStream, read []byte) (runs [][2]int, at map[int]int) {
	sig := func(p []byte) string { return string(p[64:96]) }
	which := map[string]int{}
	for f := range s.frames {
		which[sig(s.data[s.stamped(f):])] = f
	}
	at = map[int]int{}
	for i := 0; i+188 <= len(read); i += 188 {
		p := read[i : i+188]
		if p[0] != 0x47 || p[1] != 0x41 || p[2] != 0 {
			continue
		}
		f, ok := which[sig(p)]
		if !ok {
			f = -1
		}
		at[f] = i
		if n := len(runs); n > 0 && f == runs[n-1][1]+1 {
			runs[n-1][1] = f
		} else {
			runs = append(runs, [2]int{f, f})
		}
	}
	return
}

// wreckSeen: wreckRuns for a log line.
func wreckSeen(s *wreckStream, read []byte) string {
	runs, _ := wreckRuns(s, read)
	var out []string
	for _, r := range runs[:min(len(runs), 40)] {
		out = append(out, fmt.Sprintf("%d-%d", r[0], r[1]))
	}
	return strings.Join(out, " ")
}

// wreckOwed: by when (the provider sent it at at) a device should have read off bytes in all.
type wreckOwed struct {
	at  time.Time
	off int
}

// wreckOwedOf: what the device is owed for each write of the provider's that it was sent the end of.
// skip: writes that end at these places in the stream are left out.
func wreckOwedOf(segs []wreckSeg, sends []wreckSend, skip func(off int) bool) (owed []wreckOwed) {
	for _, sg := range segs {
		for _, x := range sends {
			if x.conn == sg.conn && x.to > sg.from && x.to <= sg.to+187 && (skip == nil || !skip(x.to)) {
				owed = append(owed, wreckOwed{x.at, sg.at + x.to - sg.from})
			}
		}
	}
	return
}

// wreckWorst: the longest any of it took to reach the device, and how much never did. (The start of a packet
// whose rest has not come may wait for the rest.)
func wreckWorst(owed []wreckOwed, marks []wreckMark) (worst time.Duration, at, never int) {
	k := 0
	for _, o := range owed {
		for k < len(marks) && marks[k].off < o.off-187 {
			k++
		}
		if k == len(marks) {
			never++
			continue
		}
		if d := marks[k].at.Sub(o.at); d > worst {
			worst, at = d, o.off
		}
	}
	return
}

// wreckClockRuns: the clock in what a device was sent runs on. Every packet is on the 188-byte grid. The PCR
// never steps back and never leaps (it comes with every frame, 40 ms apart); no decode time steps back more
// than 50 ms; no PTS more than 1 s (pictures are shown in another order than sent). A step ahead is allowed
// only at joins: the places where a stream asked for again was joined on, where the provider's own drop left
// video out. fills: packets a drop cut short (their timestamps may be cut too).
func wreckClockRuns(t *testing.T, out []byte, joins, fills []int) {
	t.Helper()
	off := 0
	for i := 0; i+188 <= len(out); i += 188 {
		if out[i] != 0x47 {
			off++
		}
	}
	if off > 0 {
		t.Errorf("%d of the %d packets the device was sent do not start where a packet should: the stream left its 188-byte grid", off, len(out)/188)
	}
	st := ecStamps(out)
	keys, vals, idx := ecLines(st)
	last, lastAt := map[ecLine]int64{}, map[ecLine]int{}
	name := map[byte]string{'C': "PCR", 'T': "decode time (DTS)", 'P': "PTS"}
	n := 0
	for k, key := range keys {
		at := st[idx[k]].at
		if slices.Contains(fills, at) {
			continue
		}
		p, seen := last[key]
		from := lastAt[key]
		last[key], lastAt[key] = vals[k], at
		if !seen {
			continue
		}
		back, ahead := 0.0, 0.1
		switch key.kind {
		case 'T':
			back = -0.05
		case 'P':
			back, ahead = -1, 0.3
		}
		joined := false
		for _, j := range joins {
			joined = joined || (from < j && j <= at)
		}
		if d := ecDiff(vals[k], p); d < back || (d > ahead && !joined) {
			if n++; n <= 4 {
				t.Errorf("the device's %s on PID %d went from %.3f s to %.3f s (%+.3f s) at byte %d of what it read",
					name[key.kind], key.pid, float64(p)/90000, float64(vals[k])/90000, d, at)
			}
		}
	}
	if n > 4 {
		t.Errorf("... %d such steps in all", n)
	}
}

// wreckSpacing: every PTS and DTS is as far from the PCR before it as the provider sent it (sound and
// picture stay together, and with the clock).
func wreckSpacing(t *testing.T, in, out []byte) {
	t.Helper()
	a, b := ecStamps(in), ecStamps(out)
	if len(a) != len(b) {
		t.Errorf("the provider sent %d timestamps, the device was sent %d", len(a), len(b))
		return
	}
	pa, pb, n := int64(-1), int64(-1), 0
	for k := range a {
		if a[k].kind == 'C' {
			pa, pb = a[k].v, b[k].v
		} else if pa >= 0 && ecDiff(a[k].v, pa) != ecDiff(b[k].v, pb) {
			if n++; n <= 3 {
				t.Errorf("at byte %d a timestamp on PID %d is %+.3f s from the PCR; the provider sent it %+.3f s from it", b[k].at, b[k].pid, ecDiff(b[k].v, pb), ecDiff(a[k].v, pa))
			}
		}
	}
	if n > 3 {
		t.Errorf("... %d such timestamps in all", n)
	}
}

// ---------------------------------------------------------------------------
// One device watching through it all
// ---------------------------------------------------------------------------

// wreckRun: one device watched the channel p serves.
type wreckRun struct {
	p   *wreckProv
	v   *wreckView
	end float64
	err error
}

// wreckGo: one device watches for end seconds or until its stream ends (end 0: until the provider has sent
// everything and the last of it had time to arrive).
func wreckGo(t *testing.T, p *wreckProv, end float64) *wreckRun {
	base := wreckFront(t, p)
	p.start = time.Now()
	v, err := wreckWatch(base, "192.0.2.50")
	if err != nil {
		return &wreckRun{p: p, err: err}
	}
	if end > 0 {
		select {
		case <-v.done:
		case <-time.After(time.Until(p.start.Add(secs(end)))):
		}
	} else {
		select {
		case <-v.done:
		case <-p.finished:
			time.Sleep(1500 * time.Millisecond)
		case <-time.After(120 * time.Second):
		}
	}
	v.stop()
	return &wreckRun{p: p, v: v, end: time.Since(p.start).Seconds()}
}

// checked: the device was sent the provider's bytes (wreckFollow), none of them late, on a clock that runs
// on. asks: how many times the provider may have been asked (nil: not checked).
func (r *wreckRun) checked(t *testing.T, asks []int, stamps bool, skip func(off int) bool) []wreckSeg {
	t.Helper()
	if r.err != nil {
		t.Fatalf("asking for the channel: %v", r.err)
	}
	if r.v.status != 200 {
		t.Fatalf("the channel: %d", r.v.status)
	}
	p, v := r.p, r.v
	p.mu.Lock()
	sends, asked := append([]wreckSend(nil), p.sends...), append([]float64(nil), p.asks...)
	p.mu.Unlock()
	if len(sends) == 0 || len(v.marks) == 0 {
		t.Fatalf("nothing was sent (%d writes) or nothing arrived (%d reads)", len(sends), len(v.marks))
	}
	if asks != nil && !slices.Contains(asks, len(asked)) {
		t.Errorf("the provider was asked %d times (at %.1f s), want %v", len(asked), asked, asks)
	}
	segs, fills, ok := wreckFollow(t, p.fd.wreckStream, sends, v.read, stamps)
	if !ok || t.Failed() {
		t.Logf("the video frames the device was sent (%d a second, by their number in the channel): %s", wreckFPS, wreckSeen(p.fd.wreckStream, v.read))
	}
	if !ok {
		t.FailNow()
	}
	worst, at, never := wreckWorst(wreckOwedOf(segs, sends, skip), v.marks)
	gapP, gapV := time.Duration(0), time.Duration(0)
	for k := 1; k < len(sends); k++ {
		gapP = max(gapP, sends[k].at.Sub(sends[k-1].at))
	}
	for k := 1; k < len(v.marks); k++ {
		gapV = max(gapV, v.marks[k].at.Sub(v.marks[k-1].at))
	}
	t.Logf("%d bytes in %d writes on %d connections; the device was at most %d ms behind the provider (at byte %d); longest time with nothing sent: provider %.1f s, device %.1f s",
		len(v.read), len(sends), len(segs), worst.Milliseconds(), at, gapP.Seconds(), gapV.Seconds())
	if never > 0 {
		t.Errorf("%d of the provider's writes never reached the device", never)
	}
	if worst > wreckLate {
		t.Errorf("video was held: bytes the provider had sent reached the device %d ms later (at byte %d of what it read); at most %d ms is allowed",
			worst.Milliseconds(), at, wreckLate.Milliseconds())
	}
	if len(segs) == 1 && gapV > gapP+wreckLate {
		t.Errorf("the device went %.1f s without video; the provider never stopped for more than %.1f s", gapV.Seconds(), gapP.Seconds())
	}
	var joins []int
	for _, sg := range segs[1:] {
		joins = append(joins, sg.at)
	}
	wreckClockRuns(t, v.read, joins, fills)
	return segs
}

// askedAgain: after each drop the channel was asked for again when the design says, and that ask was served.
// outage: how long after a drop the provider refuses to serve.
func (r *wreckRun) askedAgain(t *testing.T, segs []wreckSeg, outage float64) {
	t.Helper()
	due := dropGap
	for i := 0; due < outage && i < len(reaskBackoff); i++ {
		due += reaskBackoff[i]
	}
	for i, d := range r.p.dropped {
		if i+1 >= len(segs) {
			t.Errorf("the provider dropped the channel at %.1f s and it never played again", d)
			return
		}
		at := r.p.asks[segs[i+1].conn-1] - d
		resumed := 0.0
		for _, m := range r.v.marks {
			if m.off > segs[i+1].at {
				resumed = m.at.Sub(r.p.start).Seconds() - d
				break
			}
		}
		t.Logf("drop %d at %.1f s: the ask that was served came %.1f s later, the device's video carried on %.1f s after the drop", i+1, d, at, resumed)
		if at > due+1 {
			t.Errorf("drop %d: the ask that was served came %.1f s after the drop, want %.1f s", i+1, at, due)
		}
	}
}

// smooth: a player on the device never ran out of video (it starts once it holds 2 s) and never held under a
// second when a lump came.
func (r *wreckRun) smooth(t *testing.T) {
	t.Helper()
	lv := r.v.player(r.p.start)
	first, least, stalls, stalled := lv.play(2, 1, r.end)
	t.Logf("a player: first video %.1f s after asking, fewest seconds in hand when a lump came %.1f, %d stalls (%.1f s), video left out at joins %.1f s",
		first, least, stalls, stalled, lv.skips)
	if first < 0 || first > 5 {
		t.Errorf("playing %.1f s after asking, want within 5 s", first)
	}
	if least < 1 {
		t.Errorf("a player held %.1f s of video when a lump came, want 1 s or more", least)
	}
	if stalls > 0 {
		t.Errorf("a player ran out of video %d times (%.1f s)", stalls, stalled)
	}
	if lv.back > 0 {
		t.Errorf("the stream's clock went backwards %d times", lv.back)
	}
}

// playsOn: after an outage on the provider's side a player has video again (it may have run dry meanwhile:
// the provider had none to send), with no step back.
func (r *wreckRun) playsOn(t *testing.T, within float64) {
	t.Helper()
	lv := r.v.player(r.p.start)
	first, least, stalls, stalled := lv.play(2, 1, r.end)
	t.Logf("a player: first video %.1f s after asking, fewest seconds in hand when a lump came %.1f, %d stalls (%.1f s), video left out at joins %.1f s",
		first, least, stalls, stalled, lv.skips)
	if lv.back > 0 {
		t.Errorf("the stream's clock went backwards %d times", lv.back)
	}
	if n := len(lv.t); n == 0 || lv.t[n-1] < r.end-within {
		t.Errorf("no video in the last %.0f s", within)
	}
}

// ---------------------------------------------------------------------------
// The scenes
// ---------------------------------------------------------------------------

// wreckScene: run sends a feed and watches it (t there is the whole test, for setting up; it must not end
// the test), and returns the checks, which run as a subtest of that name.
type wreckScene struct {
	name string
	run  func(t *testing.T) func(t *testing.T)
}

// wreckPicked: the scenes -run asks for (go test -run 'TestLiveWreck/^drop' starts only those).
func wreckPicked() func(name string) bool {
	all := func(string) bool { return true }
	for _, arg := range os.Args {
		parts := strings.Split(strings.TrimPrefix(arg, "-test.run="), "/")
		if !strings.HasPrefix(arg, "-test.run=") || len(parts) < 2 {
			continue
		}
		if re, err := regexp.Compile(parts[1]); err == nil {
			return func(name string) bool { return re.MatchString(strings.ReplaceAll(name, " ", "_")) }
		}
	}
	return all
}

// wreckBroke: the checks of a scene that could not be run.
func wreckBroke(e any) func(*testing.T) {
	return func(t *testing.T) { t.Fatalf("the scene broke down: %v", e) }
}

func TestLiveWreck(t *testing.T) {
	seed := time.Now().UnixNano() % 1e9
	if s := os.Getenv("COAXIAL_ROUGH_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("COAXIAL_ROUGH_SEED=%q is not a number", s)
		}
		seed = n
	}
	t.Logf("the seeded feeds (crowd, random) are made from seed %d: COAXIAL_ROUGH_SEED=%d sends the same ones again", seed, seed)
	var scenes []wreckScene
	scenes = append(scenes, wreckDropScenes()...)
	scenes = append(scenes, wreckClockScenes()...)
	scenes = append(scenes, wreckCrowdScene(seed))
	scenes = append(scenes, wreckRandomScenes(seed)...)
	wreckScenes(t, scenes)
}

// wreckScenes starts every scene -run asks for, all at once, then runs their checks one after another.
func wreckScenes(t *testing.T, scenes []wreckScene) {
	wreckShared()
	pick := wreckPicked()
	type going struct {
		name   string
		checks chan func(*testing.T)
	}
	var run []going
	for _, sc := range scenes {
		if !pick(sc.name) {
			continue
		}
		g := going{sc.name, make(chan func(*testing.T), 1)}
		run = append(run, g)
		go func() {
			defer func() {
				if e := recover(); e != nil {
					g.checks <- wreckBroke(e)
				}
			}()
			g.checks <- sc.run(t)
		}()
	}
	for _, g := range run {
		t.Run(g.name, func(t *testing.T) { (<-g.checks)(t) })
	}
}

// ---- drops ----

func wreckDropScenes() []wreckScene {
	// drop: a channel on the usual lumps for end seconds, with a provider set up by set.
	drop := func(name string, end float64, set func(p *wreckProv), checks func(t *testing.T, r *wreckRun)) wreckScene {
		return wreckScene{name, func(t *testing.T) func(*testing.T) {
			p := &wreckProv{fd: wreckLive(wreckShared(), 12, 16, wreckLumps(end-2)), back: 12}
			set(p)
			r := wreckGo(t, p, end)
			return func(t *testing.T) { checks(t, r) }
		}}
	}
	out := []wreckScene{
		// The provider ends the connection in the middle of a lump, in the middle of a packet, and serves the
		// channel again at once: the viewer notices nothing but the few frames up to the next keyframe.
		drop("drop, served again at once", 50, func(p *wreckProv) { p.drops = []wreckDrop{{at: 33}} },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{2}, false, nil)
				r.askedAgain(t, segs, 0)
				r.smooth(t)
			}),
		// Twice on one channel: in the middle of a lump right after a 12 s pause, then (the second connection has
		// run reopenAfter seconds by then) between two lumps, when the new connection has nothing new to send
		// until the next lump comes.
		drop("drop, twice on one channel", 74, func(p *wreckProv) { p.drops = []wreckDrop{{at: 31}, {at: 64.5, clean: true}} },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{3}, false, nil)
				r.askedAgain(t, segs, 0)
				r.smooth(t)
			}),
		// The provider restarts the channel on its side: 404 for 7 s after the drop. Asked at once, then after
		// 2, 3 and 5 s: the fourth ask is served.
		drop("drop, then 404 for 7 s", 58, func(p *wreckProv) { p.drops, p.refuse, p.refuseFor = []wreckDrop{{at: 33}}, 404, 7 },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{5}, false, nil)
				r.askedAgain(t, segs, 7)
				r.playsOn(t, 8)
			}),
		// A server error for 18 s: the sixth ask, 20 s after the drop, is served.
		drop("drop, then 503 for 18 s", 67, func(p *wreckProv) { p.drops, p.refuse, p.refuseFor = []wreckDrop{{at: 33}}, 503, 18 },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{6}, false, nil)
				r.askedAgain(t, segs, 18)
				r.playsOn(t, 8)
			}),
		// The new connection is answered at once and then sends nothing for 8 s: it is waited for (up to twice
		// reopenWait), not given up on.
		drop("drop, then a new connection quiet for 8 s", 56, func(p *wreckProv) { p.drops, p.quietEach = []wreckDrop{{at: 33}}, 8 },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{2}, false, nil)
				r.askedAgain(t, segs, 0)
				r.playsOn(t, 8)
			}),
		// The channel has no video for 10 s after the drop, whoever asks. The first new connection is given up
		// on after twice reopenWait, about when video comes; if it was, the next ask, 2 s later, has video.
		drop("drop, then no video for 10 s", 60, func(p *wreckProv) { p.drops, p.quietAll = []wreckDrop{{at: 33}}, 10 },
			func(t *testing.T, r *wreckRun) {
				segs := r.checked(t, []int{2, 3}, false, nil)
				if len(segs) > 1 {
					resumed := 0.0
					for _, m := range r.v.marks {
						if m.off > segs[1].at {
							resumed = m.at.Sub(r.p.start).Seconds() - r.p.dropped[0]
							break
						}
					}
					t.Logf("the device's video carried on %.1f s after the drop", resumed)
					if resumed == 0 || resumed > dropGap+2*reopenWait+reaskBackoff[0]+1.5 {
						t.Errorf("the device's video carried on %.1f s after the drop; the provider had video again after 10 s", resumed)
					}
				} else {
					t.Errorf("the channel never played again")
				}
				r.playsOn(t, 8)
			}),
		// A stream that ran under reopenAfter is not asked for again (that protects accounts with one
		// connection). The device is let go at once, so that its player can ask again itself, with every byte
		// the provider sent before the drop.
		{"drop after 14 s, not asked for again", func(t *testing.T) func(*testing.T) {
			p := &wreckProv{fd: wreckLive(wreckShared(), 12, 16, wreckLumps(30)), back: 12, drops: []wreckDrop{{at: 12}}}
			r := wreckGo(t, p, 30)
			time.Sleep(3 * time.Second)
			return func(t *testing.T) {
				r.checked(t, []int{1}, false, nil)
				sent := r.p.sends[len(r.p.sends)-1]
				if len(r.v.read) != sent.to {
					t.Errorf("the provider sent %d bytes before its drop, the device was sent %d", sent.to, len(r.v.read))
				}
				if r.v.ended.IsZero() || r.v.ended.Sub(sent.at) > time.Second {
					t.Errorf("the device was not let go within a second of the drop")
				}
			}
		}},
	}
	// A 24/7 channel moves on to its next episode (its clock starts over), and 12 s later the provider drops
	// it. The new connection is on the new episode's clock; the viewer's clock carries on through both.
	out = append(out, wreckScene{"drop after an episode change", func(t *testing.T) func(*testing.T) {
		s := wreckBuild(8, wreckPart{secs: 40, clock: 5000}, wreckPart{secs: 60, clock: 0.7, file: true})
		jump := s.frames[s.parts[1]]
		p := &wreckProv{fd: wreckLive(s, 12, 16, wreckLumps(54), s.stamped(s.parts[1])), back: 12, drops: []wreckDrop{{at: 33}}}
		r := wreckGo(t, p, 56)
		return func(t *testing.T) {
			segs := r.checked(t, []int{2}, true, func(off int) bool { return off > jump && off < s.frames[s.parts[1]+7] })
			r.askedAgain(t, segs, 0)
			if n := min(jump, len(r.v.read)); !bytes.Equal(r.v.read[:n], s.data[:n]) {
				t.Errorf("the stream was changed before its clock started over")
			}
			r.smooth(t)
		}
	}})
	// The same, but the drop comes 2 s of video after the change, so the new connection's opening burst starts
	// in the old episode, on the old clock, and runs through the change again. That is not joined on at a
	// keyframe: the clock code sees video it has passed on come again and leaves it out, frame for frame
	// (replay.go). The viewer carries on with the very frame the drop cut off: no frame of the channel left
	// out, none sent twice but that one, on a clock that never steps.
	out = append(out, wreckScene{"drop just after an episode change", func(t *testing.T) func(*testing.T) {
		s := wreckBuild(9, wreckPart{secs: 46, clock: 5000}, wreckPart{secs: 54, clock: 0.7, file: true})
		p := &wreckProv{fd: wreckLive(s, 12, 16, wreckLumps(54), s.stamped(s.parts[1])), back: 12, drops: []wreckDrop{{at: 33}}}
		r := wreckGo(t, p, 56)
		return func(t *testing.T) {
			if r.err != nil || r.v.status != 200 {
				t.Fatalf("asking for the channel: %v, status %d", r.err, r.v.status)
			}
			if len(p.asks) != 2 || len(p.dropped) != 1 || p.asks[1]-p.dropped[0] > dropGap+1 {
				t.Fatalf("the provider dropped the channel at %.1f s and was asked at %.1f s, want twice, the second time at once", p.dropped, p.asks)
			}
			runs, at := wreckRuns(s, r.v.read)
			sent := p.sends[len(p.sends)-1]
			t.Logf("the video frames the device was sent: %s; the provider's last write ended in frame %d", wreckSeen(s, r.v.read), s.frameOf(sent.to-1))
			if len(runs) != 2 || runs[0][0] != 0 || (runs[1][0] != runs[0][1] && runs[1][0] != runs[0][1]+1) || runs[1][1] < s.frameOf(sent.to-1)-1 {
				t.Errorf("want every frame of the channel once, in order, and at most the one the drop cut off twice")
			}
			var owed []wreckOwed
			for _, x := range p.sends {
				if f := s.frameOf(x.to-1) - 1; x.conn == 2 && f > runs[0][1] {
					owed = append(owed, wreckOwed{x.at, at[f] + 188})
				}
			}
			if worst, where, never := wreckWorst(owed, r.v.marks); worst > wreckLate || never > 0 || len(owed) == 0 {
				t.Errorf("video was held: of %d writes of new video after the drop, the worst reached the device %d ms later (at byte %d) and %d never did", len(owed), worst.Milliseconds(), where, never)
			}
			wreckClockRuns(t, r.v.read, nil, nil)
			r.smooth(t)
		}
	}})
	if wreckLong() {
		out = append(out,
			// (the channel is 36 s further on when it plays again: more than the 30 s the clock code takes for a
			// jump, so the viewer's clock carries on as if nothing were missing; timestamps are not compared)
			drop("drop, then 404 for 40 s", 90, func(p *wreckProv) { p.drops, p.refuse, p.refuseFor = []wreckDrop{{at: 33}}, 404, 40 },
				func(t *testing.T, r *wreckRun) {
					segs := r.checked(t, []int{8}, true, nil)
					r.askedAgain(t, segs, 40)
					r.playsOn(t, 8)
				}))
	}
	return out
}

// ---- clocks ----

// wreckClockScene: a stream sent in lumps, quickly (what matters is the order of the bytes and where reads
// end, not the pauses). cut: a write ends this many bytes into the first packet of the second part that
// carries timestamps, and the rest comes 300 ms later (-1: no such write; that packet then comes in the
// middle of a lump, or, with edge, at the start of one).
func wreckClockScene(name string, cut int, edge bool, parts ...wreckPart) wreckScene {
	return wreckScene{name, func(t *testing.T) func(*testing.T) {
		s := wreckBuild(uint64(len(name)), parts...)
		plan, jump, hold := wreckClockPlan(s, cut, edge)
		p := &wreckProv{fd: wreckPlanned(s, plan)}
		r := wreckGo(t, p, 0)
		return func(t *testing.T) { wreckClockChecks(t, r, s, jump, hold) }
	}}
}

// wreckClockPlan: 8 s of the stream at once, then 3.5 s of it every 350 ms. jump: where the second part
// starts (-1: one part); hold: where the frames that may be held after it end.
func wreckClockPlan(s *wreckStream, cut int, edge bool) (plan []roughStep, jump, hold int) {
	jump, hold = -1, -1
	stamped := -1
	if len(s.parts) > 1 {
		jump, hold, stamped = s.frames[s.parts[1]], s.frames[s.parts[1]+7], s.stamped(s.parts[1])
	}
	from := 0
	add := func(wait time.Duration, to int) {
		if to > from {
			plan, from = append(plan, roughStep{wait, to - from}), to
		}
	}
	lump := func(wait time.Duration, to int) {
		if stamped < from || stamped >= to {
			add(wait, to)
			return
		}
		// (the packet the clock jumps in comes in a write of its own, so that no read ends in its first bytes)
		add(wait, stamped-8<<10)
		if cut >= 0 {
			add(60*time.Millisecond, stamped+cut)
			add(300*time.Millisecond, min(to, stamped+16<<10))
		} else {
			add(60*time.Millisecond, min(to, stamped+16<<10))
		}
		add(0, to)
	}
	step := 3.5
	if edge {
		step = 2
	}
	wait := time.Duration(0)
	for c := 8.0; ; c += step {
		f := int(c * wreckFPS)
		if f >= len(s.frames) {
			lump(wait, len(s.data))
			return
		}
		lump(wait, s.frames[f])
		wait = 350 * time.Millisecond
	}
}

func wreckClockChecks(t *testing.T, r *wreckRun, s *wreckStream, jump, hold int) {
	t.Helper()
	if jump < 0 { // the clock runs on: byte for byte
		r.checked(t, []int{1}, false, nil)
		if !bytes.Equal(r.v.read, s.data) {
			t.Errorf("a stream whose clock never jumped was not passed on byte for byte (%d bytes sent, %d arrived)", len(s.data), len(r.v.read))
		}
		return
	}
	if at, from, to := ecFirstJump(ecStamps(s.data)); at < 0 {
		t.Fatalf("the provider's stream has no clock jump; nothing to test")
	} else {
		t.Logf("the provider's clock jumps at byte %d (%.3f s, then %.3f s)", at, from, to)
	}
	r.checked(t, []int{1}, true, func(off int) bool { return off > jump && off < hold })
	if len(r.v.read) != len(s.data) {
		t.Errorf("the provider sent %d bytes, the device was sent %d: video was added or left out", len(s.data), len(r.v.read))
	}
	if n := min(jump, len(r.v.read)); !bytes.Equal(r.v.read[:n], s.data[:n]) {
		t.Errorf("the stream was changed before its clock jumped")
	}
	if len(r.v.read) == len(s.data) {
		wreckSpacing(t, s.data, r.v.read)
	}
}

func wreckClockScenes() []wreckScene {
	episode := []wreckPart{{secs: 20, clock: 1326.4}, {secs: 20, clock: 0.7, file: true}}
	ahead := []wreckPart{{secs: 20, clock: 1000}, {secs: 20, clock: 1115}}
	out := []wreckScene{
		wreckClockScene("clock runs on, byte for byte", -1, false, wreckPart{secs: 30, clock: 1000}),
		// (a channel up for 26.5 hours: the 33-bit clock goes round; that is not a jump)
		wreckClockScene("clock wraps after 26 hours, byte for byte", -1, false, wreckPart{secs: 30, clock: float64(tsWrap)/90000 - 14}),
		wreckClockScene("clock starts over in the middle of a lump", -1, false, episode...),
		wreckClockScene("clock starts over at the start of a lump", -1, true, episode...),
		wreckClockScene("clock starts over, a read ends inside the packet", 100, false, episode...),
		wreckClockScene("clock jumps ahead 95 s", -1, false, ahead...),
	}
	if wreckLong() {
		out = append(out,
			wreckClockScene("clock starts over, a read ends at the packet", 0, false, episode...),
			wreckClockScene("clock starts over, a read ends late in the packet", 187, false, episode...),
			wreckClockScene("clock jumps ahead 2 hours", -1, false, wreckPart{secs: 20, clock: 1000}, wreckPart{secs: 20, clock: 8220}),
			wreckClockScene("clock jumps ahead, a read ends inside the packet", 60, false, ahead...))
	}
	return out
}

// ---- a crowd ----

// One device watches a channel for 44 s while twelve others come and go at times drawn from the seed: some
// stay for seconds, some are gone within the second (a viewer going past the channel), three come back.
// The one that stays must notice none of it. What the design gives a joiner (join in main.go): at once, the
// newest joinSeconds of the channel by its measured speed (all that is kept, in the channel's first seconds),
// starting at the program tables before a keyframe. So a joiner here holds 10 s of video less at most one
// keyframe interval (2 s) the moment it is answered; 7 s is asked for, within a second of asking.
func wreckCrowdScene(seed int64) wreckScene {
	type visit struct {
		dev      string
		at, stay float64
		v        *wreckView
		err      error
		left     time.Time
	}
	return wreckScene{"crowd, twelve devices come and go", func(t *testing.T) func(*testing.T) {
		const end = 44.0
		rnd := rand.New(rand.NewSource(seed))
		s := wreckShared()
		p := &wreckProv{fd: wreckLive(s, 12, 16, wreckLumps(end-2))}
		base := wreckFront(t, p)
		var visits []*visit
		for d := 0; d < 12; d++ {
			x := &visit{dev: fmt.Sprintf("192.0.2.%d", 101+d), at: 0.5 + rnd.Float64()*35, stay: 2 + rnd.Float64()*12}
			if rnd.Intn(10) < 3 {
				x.stay = 0.05 + rnd.Float64()*0.6
			}
			x.stay = min(x.stay, end-1-x.at)
			visits = append(visits, x)
			if d < 3 && x.at+x.stay+3 < end-4 { // it comes back
				visits = append(visits, &visit{dev: x.dev, at: x.at + x.stay + 1 + rnd.Float64()*2, stay: 2 + rnd.Float64()*6})
			}
		}
		p.start = time.Now()
		steady, err := wreckWatch(base, "192.0.2.50")
		if err != nil {
			return func(t *testing.T) { t.Fatalf("asking for the channel: %v", err) }
		}
		var wg sync.WaitGroup
		for _, x := range visits {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(p.start.Add(secs(x.at))))
				if x.v, x.err = wreckWatch(base, x.dev); x.err == nil {
					time.Sleep(time.Until(x.v.asked.Add(secs(x.stay))))
					x.left = time.Now()
					x.v.stop()
				}
			}()
		}
		wg.Wait()
		time.Sleep(time.Until(p.start.Add(secs(end))))
		steady.stop()
		r := &wreckRun{p: p, v: steady, end: end}
		return func(t *testing.T) {
			t.Logf("COAXIAL_ROUGH_SEED=%d", seed)
			r.checked(t, []int{1}, false, nil)
			r.smooth(t)
			least := math.Inf(1)
			for _, x := range visits {
				who := fmt.Sprintf("the device that asked at %.1f s and stayed %.1f s", x.at, x.stay)
				if x.err != nil {
					t.Errorf("%s: no channel: %v", who, x.err)
					continue
				}
				if x.v.status != 200 {
					t.Errorf("%s: no channel: status %d", who, x.v.status)
					continue
				}
				v := x.v
				if x.stay < 1 || len(v.read) < 3*188 {
					continue
				}
				at := bytes.Index(s.data, v.read[:3*188])
				if at < 0 || !bytes.Equal(v.read, s.data[at:min(len(s.data), at+len(v.read))]) {
					t.Errorf("%s was not sent the provider's bytes, in order, from where it joined", who)
					continue
				}
				if f := s.frameOf(at); s.frames[f] != at || !s.key[f] {
					t.Errorf("%s was started %.2f s into the channel, which is not at a keyframe's program tables", who, float64(f)/wreckFPS)
				}
				inHand, n := 0.0, 0
				for _, m := range v.marks {
					if m.at.Sub(v.asked) <= time.Second {
						n = m.off
					}
				}
				if n > 0 {
					inHand = float64(s.frameOf(at+n-1)-s.frameOf(at)) / wreckFPS
				}
				least = min(least, inHand)
				if inHand < 7 {
					t.Errorf("%s held %.1f s of video a second after asking, want 7 s or more (first byte after %d ms)", who, inHand, v.marks[0].at.Sub(v.asked).Milliseconds())
				}
				var owed []wreckOwed
				for _, snd := range p.sends {
					if snd.to > at && snd.at.After(v.asked.Add(time.Second)) && snd.at.Before(x.left.Add(-time.Second)) {
						owed = append(owed, wreckOwed{snd.at, snd.to - at})
					}
				}
				if worst, where, never := wreckWorst(owed, v.marks); worst > wreckLate || never > 0 {
					t.Errorf("%s: video was held: bytes the provider had sent reached it %d ms later (at byte %d), %d writes never did", who, worst.Milliseconds(), where, never)
				}
			}
			t.Logf("%d visits by 12 devices; a joiner held at least %.1f s of video a second after asking", len(visits), least)
		}
	}}
}

// ---- feeds from a seed ----

// wreckRandomPlan: a provider's writes drawn from rnd: lumps after pauses, a few bytes at a time, pieces of
// any size close together, floods; no write ends on a packet except by chance. About wall seconds, at most
// size bytes.
func wreckRandomPlan(rnd *rand.Rand, wall float64, size int) (plan []roughStep) {
	took, total := 0.0, 0
	add := func(wait float64, n int) {
		if n = min(n, size-total); n > 0 {
			plan, took, total = append(plan, roughStep{secs(wait), n}), took+wait, total+n
		}
	}
	add(0, 300<<10+rnd.Intn(1<<20))
	for took < wall && total < size {
		switch k := rnd.Intn(100); {
		case k < 30: // a lump after a pause
			pause := 0.3 + rnd.Float64()*2.5
			if wreckLong() && rnd.Intn(4) == 0 {
				pause = 6 + rnd.Float64()*10 // (up to 16 s: under liveQuiet, as at peak load)
			}
			add(pause, 100<<10+rnd.Intn(1200<<10))
		case k < 55: // a few bytes at a time
			for i, n := 0, 20+rnd.Intn(60); i < n; i++ {
				add(0.001+rnd.Float64()*0.007, 1+rnd.Intn(600))
			}
		case k < 85: // pieces of any size, close together
			for i, n := 0, 3+rnd.Intn(12); i < n; i++ {
				add(rnd.Float64()*0.06, 1+rnd.Intn(64<<10))
			}
		default: // as fast as it can
			for i, n := 0, 5+rnd.Intn(30); i < n; i++ {
				add(0, 64<<10)
			}
		}
	}
	return
}

func wreckRandomScenes(seed int64) []wreckScene {
	feeds, drops, clocks, wall := 3, 1, 2, 14.0
	if wreckLong() {
		feeds, drops, clocks, wall = 8, 3, 6, 60
	}
	var out []wreckScene
	// A channel whose clock runs on, sent any way a provider might: it comes out byte for byte, as it comes.
	for i := 0; i < feeds; i++ {
		out = append(out, wreckScene{fmt.Sprintf("random feed %d", i+1), func(t *testing.T) func(*testing.T) {
			rnd := rand.New(rand.NewSource(seed + int64(i)*7919 + 1))
			s := wreckShared()
			plan := wreckRandomPlan(rnd, wall, len(s.data))
			r := wreckGo(t, &wreckProv{fd: wreckPlanned(s, plan)}, 0)
			return func(t *testing.T) {
				t.Logf("COAXIAL_ROUGH_SEED=%d: %d writes", seed, len(plan))
				r.checked(t, []int{1}, false, nil)
			}
		}})
	}
	// A live channel with lumps at times drawn from the seed, dropped once past reopenAfter: in a pause or in a
	// lump, cut anywhere, the new connection starting 3 to 13 s back.
	for i := 0; i < drops; i++ {
		out = append(out, wreckScene{fmt.Sprintf("random drop %d", i+1), func(t *testing.T) func(*testing.T) {
			rnd := rand.New(rand.NewSource(seed + int64(i)*104729 + 2))
			lead := 10 + rnd.Float64()*6
			d := wreckDrop{at: 31 + rnd.Float64()*6, clean: rnd.Intn(3) == 0, cut: 1 + rnd.Intn(60000)}
			end := d.at + 19
			lumps := []float64{0, 1 + rnd.Float64()*2}
			for at := lumps[1] + 4 + rnd.Float64()*5; at < end-1.5; at += 4 + rnd.Float64()*5 {
				lumps = append(lumps, at)
			}
			p := &wreckProv{fd: wreckLive(wreckShared(), 8+rnd.Float64()*2, lead, lumps), back: 3 + rnd.Float64()*10, drops: []wreckDrop{d}}
			r := wreckGo(t, p, end)
			return func(t *testing.T) {
				t.Logf("COAXIAL_ROUGH_SEED=%d: lumps at %.1f s, %.1f s ahead; a drop at %.1f s (between lumps: %v, %d bytes into the lump); the new connection starts %.1f s back",
					seed, lumps, lead, d.at, d.clean, d.cut, p.back)
				segs := r.checked(t, []int{2}, false, nil)
				r.askedAgain(t, segs, 0)
				r.playsOn(t, 11)
			}
		}})
	}
	// A channel whose clock starts over or leaps ahead at a place drawn from the seed, sent in writes drawn
	// from it, one of which may end inside the packet the clock jumps in (past its timestamps).
	for i := 0; i < clocks; i++ {
		out = append(out, wreckScene{fmt.Sprintf("random clock %d", i+1), func(t *testing.T) func(*testing.T) {
			rnd := rand.New(rand.NewSource(seed + int64(i)*15485863 + 3))
			a := wreckPart{secs: 2 * (4 + rnd.Intn(5)), clock: 20 + rnd.Float64()*80000}
			b := wreckPart{secs: 10, clock: rnd.Float64() * 2, file: true}
			if rnd.Intn(2) == 0 {
				b = wreckPart{secs: 10, clock: a.clock + float64(a.secs) + 30.5 + rnd.Float64()*9000}
			}
			s := wreckBuild(uint64(seed)+uint64(i), a, b)
			jump, stamped := s.frames[s.parts[1]], s.stamped(s.parts[1])
			cut := -1
			if rnd.Intn(2) == 0 {
				cut = 32 + rnd.Intn(156)
			}
			var plan []roughStep
			from := 0
			for _, st := range wreckRandomPlan(rnd, 3, stamped-8<<10) { // up to shortly before the jump
				plan, from = append(plan, st), from+st.n
			}
			plan = append(plan, roughStep{60 * time.Millisecond, stamped - 8<<10 - from})
			if cut >= 0 {
				plan = append(plan, roughStep{60 * time.Millisecond, 8<<10 + cut}, roughStep{secs(rnd.Float64() * 0.5), 16<<10 - cut})
			} else {
				plan = append(plan, roughStep{60 * time.Millisecond, 24 << 10})
			}
			plan = append(plan, wreckRandomPlan(rnd, 3, len(s.data)-stamped-16<<10)...)
			if n := roughTotal(plan); n < len(s.data) { // (whatever is left, in one more lump)
				plan = append(plan, roughStep{secs(rnd.Float64() * 0.3), len(s.data) - n})
			}
			r := wreckGo(t, &wreckProv{fd: wreckPlanned(s, plan)}, 0)
			return func(t *testing.T) {
				t.Logf("COAXIAL_ROUGH_SEED=%d: %d s on a clock from %.1f s, then one from %.1f s; %d writes, one ending %d bytes into the packet the clock jumps in (-1: none)",
					seed, a.secs, a.clock, b.clock, len(plan), cut)
				wreckClockChecks(t, r, s, jump, s.frames[s.parts[1]+7])
			}
		}})
	}
	return out
}
