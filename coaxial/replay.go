package main

import (
	"fmt"
	"hash/maphash"

	"github.com/rs/zerolog"
)

// Video sent again after a clock jump. When some providers start a live stream over inside the same
// connection (a source switch, an ad break, a restart of the channel on their side), the new stream does not
// start where the old one stopped: it starts seconds back and sends again, byte for byte, video the players
// already have, then carries on. clockFix keeps the clock running on, so players do not stall, but they show
// those seconds again: the picture jumps back, every time. So clockFix remembers a fingerprint of each video
// frame it passes on (the last fpKeep), and after a jump compares the frames that come in with them. While
// they are frames already sent, in the order they were sent, they are left out, with everything sent along
// with them (sound, tables), whole packets only. The stream carries on at the first frame not sent before,
// on the clock it was first sent with, and the continuity counters carry on from the last packets passed on,
// so a player sees one unbroken stream. A jump into video not sent before (the next episode of a 24/7
// channel) is passed on as before, after holding at most a few frames to tell. Before any jump nothing is
// held or changed.

const (
	fpKeep         = 4096    // video frames remembered (over a minute at 50 frames a second)
	replayRun      = 3       // frames in a row, sent before and in the order sent, that show a replay
	replayLead     = 3       // frames after a jump that may come before such a run (a first picture that differs)
	replayLook     = 3       // frames in a row not sent before that end a replay (fewer: a damaged frame inside it)
	replayHold     = 0.5     // seconds of video at most held after a jump to tell
	replayBytes    = 8 << 20 // bytes at most held to tell
	replayHoldWall = 1.0     // seconds of stream at most anything is held while telling
	replayWall     = 60.0    // seconds a replay may take to arrive before the rest of it is passed on (a source that never catches up)
)

var fpSeed = maphash.MakeSeed()

// heldPkt: a packet held back after a jump, or bytes between packets (off -1, passed on as they are).
type heldPkt struct {
	b   []byte
	off int64 // the clock offset its timestamps carry
}

// heldFrame: a video frame held back after a jump, from its first packet up to the next frame's.
type heldFrame struct {
	at       int    // its first entry in hold
	h        uint64 // its fingerprint (once done)
	done     bool
	partial  bool  // began before the hold did: never taken for a frame sent before
	pts, off int64 // its PTS as written and the offset it was written with; pts -1: none
	pcr      int64 // the first PCR in its span, on the provider's own clock; -1: none
	key      bool  // a decoder can start at it
	seq      int64 // the frame passed on before that it matches; -1: none
}

type replays struct {
	// the video frames passed on: fingerprint, PTS as passed on, by number (n: how many so far)
	hs   [fpKeep]uint64
	pts  [fpKeep]int64
	keys [fpKeep]bool
	n    int64
	idx  map[uint64]int64

	// the frame being read
	fh    maphash.Hash
	fOn   bool
	fLen  int
	fPTS  int64
	fHeld bool   // its first packet was held
	fKey  bool   // a decoder can start at it
	fES   []byte // its first bytes, while that is not known yet
	fDone bool   // whether it is a keyframe is known

	// after a jump
	mode      int // 0: passing on, 1: telling, 2: leaving out a replay, 3: leaving out up to a keyframe
	hold      []heldPkt
	frames    []heldFrame
	lastSeq   int64   // the newest frame sent before that the replay matched
	firstSeq  int64   // the first one
	matchOff  int64   // the offset that puts the replay on the clock it was first sent with
	t0        float64 // when leaving out began
	keyPrev   int64   // mode 3: the PTS (provider's clock) of the last frame left out waiting for a keyframe; -1: none
	keySpan   int64   // and how much video that was
	sentPCR   int64   // the clock passed on before the jump
	sentPTS   int64
	preOff    int64 // the offsets before the jump
	preOffPCR int64
	dropped   int         // bytes left out
	dropN     int         // frames left out
	holdC     [2][2]int64 // while telling: the first and newest PCR, and sound PTS, held (-1: none)
	wary      bool        // the source loops, or a replay came too slowly: leave nothing out until a jump into new video

	// continuity counters: the last one passed on per PID (0x10 set once known), and what is added to the
	// provider's after a cut (0x10 set once worked out for this cut)
	cc     [8192]byte
	ccAdj  [8192]byte
	ccCut  bool
	sure   bool // the packet being routed is followed by another 188 bytes on (else: by the end of the read)
	ccPend int  // the PID of a packet passed on at the end of a read, counted once the next read shows it was one; -1: none
	ccVal  byte

	pmtPID int
	vtyp   byte

	jl        jumpLog // the last jump, logged once what came after it is known
	anchorPCR anchor  // the provider's clock before the last jump, to see it come back (an ad break ending)
	anchorPTS anchor

	// this call's output: data[:pos] went out as it was, unless copied (then res)
	data   []byte
	res    []byte
	pos    int
	copied bool
}

type logTag func(*zerolog.Event) *zerolog.Event

type jumpLog struct {
	set           bool
	on            string
	from, to, off float64
	second        bool
}

type anchor struct {
	set      bool
	raw, out int64
}

// jumpedAt: the clock (on: "PCR" or "video PTS") jumped. last: its last value passed on; raw: the provider's
// new one; off: the offset that makes it carry on.
func (c *clockFix) jumpedAt(on string, last, lastOff, raw, off int64) {
	r := c.rp
	if r.mode == 0 {
		r.sentPCR, r.sentPTS, r.preOff, r.preOffPCR = c.last, c.lastPTS, c.off, c.offPCR
	} else if r.jl.set {
		c.logJump("", 0, 0) // (a second jump while the first was being checked)
	}
	if on == "PCR" && c.last >= 0 { // (each clock remembers where it ran before its own jump)
		r.anchorPCR = anchor{true, ((c.last-c.offPCR)%tsWrap + tsWrap) % tsWrap, c.last}
	} else if on != "PCR" && c.lastPTS >= 0 {
		r.anchorPTS = anchor{true, ((c.lastPTS-c.off)%tsWrap + tsWrap) % tsWrap, c.lastPTS}
	}
	before := ((last-lastOff)%tsWrap + tsWrap) % tsWrap
	r.jl = jumpLog{set: true, on: on, from: float64(before) / 90000, to: float64(raw) / 90000,
		off: float64(tsDiff(off, 0)) / 90000, second: r.mode != 0}
	c.whole = true
	c.jumps++
}

// returned: whether the provider's clock (raw, v with the offset) just came back to where it ran before the last
// jump (an ad break ending): a step clockFix's limits may not see, back under 3 s or ahead under 30 s.
func (c *clockFix) returned(pcr bool, raw, v int64) bool {
	r := c.rp
	a, last := &r.anchorPCR, c.last
	if !pcr {
		a, last = &r.anchorPTS, c.lastPTS
	}
	if !a.set || last < 0 {
		return false
	}
	if step := tsDiff(v, last); step > -90000/2 && step < 90000 {
		return false // (it carries on)
	}
	if d := tsDiff(raw, a.raw+tsDiff(last, a.out)); d < -90000 || d > 90000 {
		return false
	}
	return true
}

// logJump writes the line for the last jump, with what came after it: how much video sent again was left out
// and where the stream carried on.
func (c *clockFix) logJump(how string, secs float64, bytes int) {
	r := c.rp
	j := r.jl
	if !j.set {
		return
	}
	r.jl.set = false
	dir, size := "back", j.from-j.to
	if size < 0 {
		dir, size = "ahead", -size
	}
	if size > 47721.9 { // (the 33-bit clock's wrap)
		size = 95443.7 - size
		if dir == "back" {
			dir = "ahead"
		} else {
			dir = "back"
		}
	}
	if how == "" {
		how = "nothing sent again"
	}
	took := 0.0 // (how long what was left out took to come: a stream started over sends it in a burst)
	if bytes > 0 {
		took = c.clock() - r.t0
	}
	e := lg.Info()
	if c.tag != nil {
		e = c.tag(e)
	}
	e.Str("on", j.on).Float64("from_s", round3(j.from)).Float64("to_s", round3(j.to)).Str("direction", dir).
		Float64("jump_s", round3(size)).Float64("offset_s", round3(j.off)).Float64("left_out_s", round1(secs)).
		Float64("left_out_mb", round1(float64(bytes)/1048576)).Float64("left_out_took_s", round1(took)).Str("carried_on_at", how).
		Msg(fmt.Sprintf("a channel's clock jumped %s %.1f s (%s %.3f -> %.3f s); its timestamps now carry on from where they were (offset %+.3f s); video sent again and left out: %.1f s, %.1f MB (%s)",
			dir, size, j.on, j.from, j.to, j.off, secs, float64(bytes)/1048576, how))
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func round3(x float64) float64 { return float64(int64(x*1000+0.5*sign(x))) / 1000 }

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

func newReplays() *replays {
	return &replays{idx: map[uint64]int64{}, pmtPID: -1, keyPrev: -1, ccPend: -1}
}

func (c *clockFix) clock() float64 {
	if c.now != nil {
		return c.now()
	}
	return mono()
}

func (r *replays) record(h uint64, pts int64, key bool) {
	s := r.n
	if s >= fpKeep {
		if old := r.hs[s%fpKeep]; r.idx[old] == s-fpKeep {
			delete(r.idx, old)
		}
	}
	r.hs[s%fpKeep], r.pts[s%fpKeep], r.keys[s%fpKeep], r.idx[h] = h, pts, key, s
	r.n++
}

func (r *replays) lookup(h uint64) int64 {
	if s, ok := r.idx[h]; ok && s >= r.n-fpKeep {
		return s
	}
	return -1
}

// ensure: from here on this call's output is built in res.
func (r *replays) ensure() {
	if !r.copied {
		r.res = append(make([]byte, 0, len(r.data)+len(r.hold)*188), r.data[:r.pos]...)
		r.copied = true
	}
}

func (r *replays) output(end int) []byte {
	if r.copied {
		return r.res
	}
	return r.data[:end]
}

// emit passes a packet on (inPlace: it is the one at data[pos:]), carrying the continuity counter on after a cut.
func (c *clockFix) emit(b []byte, inPlace bool) {
	r := c.rp
	if pid := int(b[1]&0x1f)<<8 | int(b[2]); pid != 0x1fff {
		cc := b[3] & 15
		if r.ccCut {
			if r.ccAdj[pid]&0x10 == 0 {
				r.ccAdj[pid] = 0x10
				if last := r.cc[pid]; last&0x10 != 0 {
					want := last & 15
					if b[3]&0x10 != 0 { // (a packet with payload counts on, one without repeats it)
						want++
					}
					r.ccAdj[pid] = 0x10 | (want-cc)&15
				}
			}
			cc = (cc + r.ccAdj[pid]) & 15
			b[3] = b[3]&0xf0 | cc
		}
		if inPlace && !r.sure { // (a piece the provider cut short is not counted by players: wait for the next read)
			r.ccPend, r.ccVal = pid, 0x10|cc
		} else {
			r.cc[pid] = 0x10 | cc
		}
	}
	if !inPlace || r.copied {
		r.ensure()
		r.res = append(r.res, b...)
	}
}

// ccSeen counts a packet passed on in pieces (players count it: the next read showed it whole).
func (r *replays) ccSeen(p []byte) {
	if pid := int(p[1]&0x1f)<<8 | int(p[2]); pid != 0x1fff && !r.ccCut {
		r.cc[pid] = 0x10 | p[3]&15
	}
}

// junk routes bytes between packets (data[pos:to]): passed on as they are, unless video around them is held or left out.
func (c *clockFix) junk(to int) {
	r := c.rp
	if to <= r.pos {
		return
	}
	switch r.mode {
	case 0:
		if r.copied {
			r.res = append(r.res, r.data[r.pos:to]...)
		}
	case 1:
		r.ensure()
		r.hold = append(r.hold, heldPkt{b: append([]byte(nil), r.data[r.pos:to]...), off: -1})
	default:
		r.ensure()
		r.dropped += to - r.pos
	}
	r.pos = to
}

// route sends a whole packet (its timestamps written) on its way: on, held, or left out (by decide).
func (c *clockFix) route(p []byte, jumped bool) {
	r := c.rp
	if jumped && r.mode == 0 {
		if r.n > 0 {
			r.mode, r.hold, r.frames, r.t0 = 1, r.hold[:0], r.frames[:0], c.clock()
		} else {
			c.logJump("no video passed on yet to compare", 0, 0)
		}
	}
	before := c.off
	c.see(p)
	if r.mode == 0 {
		if c.off != before { // (what was held went out just now on a new offset: this packet was written on the old)
			c.retime(p, tsDiff(c.off, before))
		}
		c.emit(p, true)
		return
	}
	r.ensure()
	if len(r.hold) == 0 {
		r.holdC = [2][2]int64{{-1, -1}, {-1, -1}}
	}
	r.hold = append(r.hold, heldPkt{b: append([]byte(nil), p...), off: c.off})
	if f := &r.frames[len(r.frames)-1]; f.pcr < 0 && p[3]&0x20 != 0 && p[4] >= 7 && p[5]&0x10 != 0 &&
		int(p[1]&0x1f)<<8|int(p[2]) == c.pcrPID {
		v := int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7
		f.pcr = ((v-c.offPCR)%tsWrap + tsWrap) % tsWrap
	}
	// Never hold long while telling: if pictures stop coming after a jump (sound only, a new picture stream), what
	// is held goes out after a second or replayBytes, whichever comes first. (While a replay is left out, only the
	// frame being read and up to replayLook more are held.)
	if r.mode == 1 && (len(r.hold)*188 >= replayBytes || r.heldFor(p, c.pcrPID) >= int64(replayHoldWall*90000)) {
		c.release(0, false)
		r.fHeld = false
	}
}

// heldFor: how much stream time (by the PCR, or by sound when no PCR comes) went by while telling, with p held.
func (r *replays) heldFor(p []byte, pcrPID int) int64 {
	k, v := -1, int64(-1)
	pid, pos := int(p[1]&0x1f)<<8|int(p[2]), 4
	if p[3]&0x20 != 0 {
		pos = 5 + int(p[4])
		if p[4] >= 7 && p[5]&0x10 != 0 && pid == pcrPID {
			k, v = 0, int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7
		}
	}
	if k < 0 && p[1]&0x40 != 0 && pos+14 <= 188 && p[pos] == 0 && p[pos+1] == 0 && p[pos+2] == 1 &&
		(p[pos+3] >= 0xc0 && p[pos+3] < 0xe0 || p[pos+3] == 0xbd) && p[pos+7]&0x80 != 0 {
		k, v = 1, readTS(p[pos+9:pos+14])
	}
	if k >= 0 {
		if r.holdC[k][0] < 0 {
			r.holdC[k][0] = v
		}
		r.holdC[k][1] = v
	}
	for _, h := range r.holdC {
		if h[0] >= 0 {
			if d := tsDiff(h[1], h[0]); d > 0 && d < 3600*90000 {
				return d
			}
		}
	}
	return 0
}

// see reads a packet for the video frames' fingerprints and the program's video type.
func (c *clockFix) see(p []byte) {
	r := c.rp
	pid := int(p[1]&0x1f)<<8 | int(p[2])
	afc, pos := p[3]>>4&3, 4
	if afc&2 != 0 {
		pos = 5 + int(p[4])
	}
	if afc&1 == 0 || pos >= 188 {
		c.holdPartial()
		return
	}
	body := p[pos:]
	start := p[1]&0x40 != 0
	switch {
	case start && pid == 0:
		if pmt := patPMT(tableStart(body)); pmt >= 0 {
			r.pmtPID = pmt
		}
	case start && pid == r.pmtPID:
		if v, typ := pickType(tableStart(body)); v >= 0 && v == c.videoPID {
			r.vtyp = typ
		}
	case pid != c.videoPID || c.videoPID < 0:
	case start && len(body) >= 9 && body[0] == 0 && body[1] == 0 && body[2] == 1 && body[3] >= 0xe0 && body[3] <= 0xef:
		c.frameEnd()
		pts := int64(-1)
		if body[7]&0x80 != 0 && len(body) >= 14 {
			pts = readTS(body[9:14])
		}
		r.fh.Reset()
		r.fh.SetSeed(fpSeed)
		r.fOn, r.fLen, r.fPTS, r.fHeld = true, 0, pts, r.mode != 0
		r.fKey = p[3]&0x20 != 0 && p[4] > 0 && p[5]&0x40 != 0
		r.fDone, r.fES = r.fKey, r.fES[:0]
		if r.fHeld {
			r.frames = append(r.frames, heldFrame{at: len(r.hold), pts: pts, off: c.off, pcr: -1, seq: -1})
		}
		es := body[min(len(body), 9+int(body[8])):]
		r.fh.Write(es)
		r.fLen += len(es)
		r.keyBytes(es)
	case r.fOn:
		r.fh.Write(body)
		r.fLen += len(body)
		r.keyBytes(body)
	}
	c.holdPartial()
}

// keyBytes reads the start of the frame being read until it is known whether a decoder can start at it.
func (r *replays) keyBytes(b []byte) {
	if r.fDone {
		return
	}
	r.fES = append(r.fES, b...)
	key, done := keyNAL(r.fES, r.vtyp)
	r.fKey, r.fDone = key, done || len(r.fES) >= 1024
}

// holdPartial: a hold that began inside a frame starts with that frame's rest.
func (c *clockFix) holdPartial() {
	if r := c.rp; r.mode != 0 && len(r.frames) == 0 {
		r.frames = append(r.frames, heldFrame{at: len(r.hold), partial: true, pts: r.fPTS, pcr: -1, seq: -1})
		r.fHeld = r.fOn
	}
}

// frameEnd: the frame being read is complete (the next one starts).
func (c *clockFix) frameEnd() {
	r := c.rp
	if !r.fOn {
		return
	}
	r.fOn = false
	h := r.fh.Sum64() ^ uint64(r.fLen)*0x9e3779b97f4a7c15
	if !r.fHeld {
		if r.fLen > 0 {
			r.record(h, r.fPTS, r.fKey)
		}
		return
	}
	f := &r.frames[len(r.frames)-1]
	f.h, f.done, f.key = h, true, r.fKey && !f.partial
	if !f.partial && r.fLen > 0 {
		f.seq = r.lookup(h)
	}
	c.decide()
}

// decide: a held frame is complete; leave out, wait, or pass on. (Called before the next frame's first
// packet is routed: everything held belongs to complete frames.)
func (c *clockFix) decide() {
	r := c.rp
	fs := r.frames
	n := len(fs)
	switch r.mode {
	case 1: // telling: a replay is replayRun frames in a row sent before, in order
		run := n >= replayRun && n-replayRun <= replayLead && !r.wary
		for k := n - replayRun; run && k < n; k++ {
			run = fs[k].seq >= 0 && (k == n-replayRun || fs[k].seq == fs[k-1].seq+1)
		}
		// (a stream started over starts at a keyframe: the copy of one sent before. Frames that merely look alike,
		// such as tiny frames of a still or black picture, do not start such a run)
		run = run && fs[n-replayRun].key && r.keys[fs[n-replayRun].seq%fpKeep]
		if run {
			r.firstSeq, r.dropped, r.dropN, r.t0 = fs[n-replayRun].seq, 0, 0, c.clock()
			c.matched(fs[n-1])
			c.dropTo(n)
			r.mode = 2
			return
		}
		span := int64(0) // (pictures come in another order than shown: the spread of their times)
		for _, f := range fs {
			if f.pts >= 0 && fs[0].pts >= 0 {
				span = max(span, abs64(tsDiff(f.pts-f.off, fs[0].pts-fs[0].off)))
			}
		}
		if n >= replayRun+replayLead || span >= int64(replayHold*90000) || len(r.hold)*188 >= replayBytes {
			if !run && fs[n-1].seq < 0 {
				r.wary = false // (a jump into new video: the source does not loop)
			}
			c.release(0, false)
		}
	case 2: // leaving out a replay
		f := fs[n-1]
		if f.seq >= 0 && f.seq <= r.lastSeq { // the replay went back: the source loops; pass it on from a keyframe
			r.wary, r.mode = true, 3
			c.seekKey()
			return
		}
		if f.seq >= 0 {
			c.matched(f)
			c.dropTo(n) // (with any frame before it not matched: a damaged copy)
			if c.clock()-r.t0 > replayWall {
				r.wary, r.mode = true, 3 // (too slow: players would run dry waiting; the rest is passed on)
			}
			return
		}
		if n < replayLook {
			return
		}
		if r.lastSeq >= r.n-3 { // the replay reached what was passed on: the next frame follows on from it
			c.release(0, true)
			return
		}
		r.mode = 3
		c.seekKey()
	case 3:
		c.seekKey()
	}
}

// seekKey: carry on at the first keyframe held, else leave out what is held (at most keyWait seconds).
func (c *clockFix) seekKey() {
	r := c.rp
	for k, f := range r.frames {
		if f.key {
			c.dropTo(k)
			c.release(0, false)
			return
		}
		if f.pts >= 0 {
			if d := tsDiff(f.pts-f.off, r.keyPrev); r.keyPrev >= 0 && d > 0 && d < 9000 {
				r.keySpan += d
			}
			r.keyPrev = f.pts - f.off
		}
		if r.keySpan >= keyWait*90000 { // (no keyframe marked: carry on anyway)
			c.dropTo(k)
			c.release(0, false)
			return
		}
	}
	c.dropTo(len(r.frames))
}

// matched: frame f is a copy of a frame sent before.
func (c *clockFix) matched(f heldFrame) {
	r := c.rp
	r.lastSeq = f.seq
	if f.pts >= 0 {
		r.matchOff = ((r.pts[f.seq%fpKeep]-(f.pts-f.off))%tsWrap + tsWrap) % tsWrap
	}
}

// dropTo leaves out the first k held frames.
func (c *clockFix) dropTo(k int) {
	r := c.rp
	if k <= 0 {
		return
	}
	end := len(r.hold)
	if k < len(r.frames) {
		end = r.frames[k].at
	}
	for _, e := range r.hold[:end] {
		r.dropped += len(e.b)
	}
	r.dropN += k
	r.hold = append(r.hold[:0], r.hold[end:]...)
	r.frames = append(r.frames[:0], r.frames[k:]...)
	for i := range r.frames {
		r.frames[i].at -= end
	}
}

// release passes on what is held from frame k on (frames before it are left out) and goes back to passing on.
// Unless anything was left out, it goes out exactly as it was written. After a cut, the clock carries on:
// on the clock the replay was first sent with (along), else 40 ms after the last clock passed on.
func (c *clockFix) release(k int, along bool) {
	r := c.rp
	c.dropTo(k)
	off := c.off
	if r.dropN > 0 {
		f := heldFrame{pts: -1}
		if len(r.frames) > 0 {
			f = r.frames[0]
		}
		switch {
		case along:
			off = r.matchOff
		case f.pts >= 0 && r.sentPTS >= 0:
			off = r.sentPTS + 3600 - (f.pts - f.off)
		}
		off = (off%tsWrap + tsWrap) % tsWrap
		if d := tsDiff(off, c.off); d != 0 {
			if c.last >= 0 {
				c.last = ((c.last+d)%tsWrap + tsWrap) % tsWrap
			}
			if c.lastPTS >= 0 {
				c.lastPTS = ((c.lastPTS+d)%tsWrap + tsWrap) % tsWrap
			}
		}
		c.offPCR = ((c.offPCR+tsDiff(off, c.off))%tsWrap + tsWrap) % tsWrap
		c.off, c.tryOff, c.tryPCR = off, -1, -1
		r.ccCut, r.ccAdj = true, [8192]byte{}
		if r.jl.set {
			r.jl.off = float64(tsDiff(off, 0)) / 90000
		}
		how := "a keyframe"
		switch {
		case along:
			how = "the next frame, which follows on from the last one passed on"
		case !f.key:
			how = "a frame that is not a keyframe (none came in time)"
		}
		c.logJump(how, float64(r.dropN)*float64(tsDiff(r.pts[r.lastSeq%fpKeep], r.pts[r.firstSeq%fpKeep]))/90000/float64(max(1, int(r.lastSeq-r.firstSeq+1))), r.dropped)
	} else {
		c.logJump("", 0, 0)
	}
	for _, e := range r.hold {
		if e.off < 0 {
			r.ensure()
			r.res = append(r.res, e.b...)
			continue
		}
		if r.dropN > 0 && e.off != off {
			c.retime(e.b, tsDiff(off, e.off))
		}
		c.emit(e.b, false)
	}
	for _, f := range r.frames {
		if !f.done {
			continue
		}
		pts := f.pts
		if pts >= 0 && !f.partial && r.dropN > 0 {
			pts = ((pts+tsDiff(off, f.off))%tsWrap + tsWrap) % tsWrap
		}
		r.record(f.h, pts, f.key)
	}
	r.mode, r.hold, r.frames, r.dropN, r.dropped, r.keyPrev, r.keySpan = 0, r.hold[:0], r.frames[:0], 0, 0, -1, 0
}

// retime adds d to the timestamps of a packet (the PCR on the clock's own stream, PES PTS and DTS), as fix
// writes them.
func (c *clockFix) retime(p []byte, d int64) {
	pid := int(p[1]&0x1f)<<8 | int(p[2])
	pos := 4
	if p[3]&0x20 != 0 {
		pos = 5 + int(p[4])
		if p[4] >= 7 && p[5]&0x10 != 0 && pid == c.pcrPID {
			v := int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7
			v = ((v+d)%tsWrap + tsWrap) % tsWrap
			p[6], p[7], p[8], p[9] = byte(v>>25), byte(v>>17), byte(v>>9), byte(v>>1)
			p[10] = p[10]&0x7f | byte(v<<7)&0x80
		}
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+14 > 188 {
		return
	}
	e := p[pos:]
	if e[0] != 0 || e[1] != 0 || e[2] != 1 || !(e[3] >= 0xc0 && e[3] <= 0xef || e[3] == 0xbd) || e[6]&0xc0 != 0x80 {
		return
	}
	flags := e[7] >> 6
	if flags&2 == 0 || pos+19 > 188 && flags == 3 {
		return
	}
	writeTS(e[9:14], readTS(e[9:14])+d)
	if flags == 3 {
		writeTS(e[14:19], readTS(e[14:19])+d)
	}
}

// reask: the provider's stream is asked for again. Half a packet of the old one is of no use, and what is
// held of it was never passed on: the clock is put back to what was.
func (c *clockFix) reask() {
	c.carry, c.next, c.tail = nil, -1, nil
	r := c.rp
	if r == nil {
		return
	}
	if r.mode != 0 {
		c.off, c.offPCR, c.last, c.lastPTS, c.tryOff, c.tryPCR = r.preOff, r.preOffPCR, r.sentPCR, r.sentPTS, -1, -1
		c.logJump("the provider's stream ended while it was checked; nothing held was passed on", 0, 0)
	}
	r.mode, r.hold, r.frames, r.fOn, r.fHeld, r.ccCut, r.dropN, r.dropped, r.keyPrev = 0, r.hold[:0], r.frames[:0], false, false, false, 0, 0, -1
}
