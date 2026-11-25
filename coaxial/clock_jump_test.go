package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// Clock jumps on a live channel shaped like a news channel: PCR and H.264 video on one PID (59.94 frames a
// second, pictures shown in another order than sent, PTS about 0.7 s past the PCR), AC-3 sound (about 0.68 s)
// and MP2 sound (about 0.49 s) on two more. What a player is sent must keep one clock: no step back, no leap
// ahead, the same distance between each sound or picture and the PCR, and no video it was already sent.

// cjPart: from content second from on, what the provider adds to its clocks.
type cjPart struct {
	from          float64
	pcr, pes, vid float64 // added to the PCR, to every PES, and to video PES only
}

type cjFeed struct {
	parts    []cjPart
	secs     float64
	rewindAt float64 // where the provider starts over rewind seconds back (sending that video again)
	rewind   float64
	restart  bool // and its clock starts over at 0.7 s with it (else its timestamps step back too)
	loop     float64
}

func cjTS(prefix byte, v float64) []byte {
	t := ((int64(math.Round(v*90000)) % tsWrap) + tsWrap) % tsWrap
	return []byte{prefix<<4 | byte(t>>29)&0x0e | 1, byte(t >> 22), byte(t>>14)&0xfe | 1, byte(t >> 7), byte(t<<1)&0xfe | 1}
}

func (fd cjFeed) build() []byte {
	var out []byte
	cc := map[int]byte{}
	pkt := func(pid int, start bool, af, payload []byte, seed int64) {
		p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10 | cc[pid]&0x0f}
		cc[pid]++
		if start {
			p[1] |= 0x40
		}
		if af != nil {
			p[3] |= 0x20
			p = append(append(p, byte(len(af))), af...)
		}
		p = append(p, payload...)
		fill := make([]byte, 188)
		rand.New(rand.NewSource(seed)).Read(fill)
		out = append(out, append(p, fill...)[:188]...)
	}
	const fd60, gop = 1001.0 / 60000, 30 // (a keyframe every half second)
	nextA, nextM := 0.0, 0.0
	shift := 0.0
	back := int(math.Round(fd.rewind/fd60/gop)) * gop // (a stream started over starts at a keyframe)
	at := int(math.Round(fd.rewindAt/fd60/gop)) * gop
	loop := int(math.Round(fd.loop/fd60/gop)) * gop
	for f := 0; float64(f)*fd60 < fd.secs+float64(back)*fd60; f++ {
		k := f
		if loop > 0 {
			k = f % loop
		}
		if fd.rewind > 0 && f >= at {
			k = f - back
		}
		t := float64(k) * fd60
		if (loop > 0 && k == 0 && f > 0) || (fd.rewind > 0 && f == at) { // the provider starts over
			nextA, nextM = math.Ceil(t/0.032)*0.032, math.Ceil(t/0.024)*0.024
			if loop > 0 || fd.restart {
				shift = 0.7 - (1000 + t)
			}
		}
		var pt cjPart
		for _, x := range fd.parts {
			if t >= x.from {
				pt = x
			}
		}
		base := 1000 + t + shift
		pcr := int64(math.Round((base + pt.pcr) * 90000))
		dts := base + 0.65 + pt.pes + pt.vid
		pts := dts + []float64{3, 0, 0}[k%3]*fd60
		af := []byte{0x10, byte(pcr >> 25), byte(pcr >> 17), byte(pcr >> 9), byte(pcr >> 1), byte(pcr&1)<<7 | 0x7e, 0}
		if k%gop == 0 {
			af[0] |= 0x40
		}
		pes := append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10}, append(cjTS(3, pts), cjTS(1, dts)...)...)
		pkt(256, true, af, pes, int64(k))
		for i := int64(1); i < 6; i++ {
			pkt(256, false, nil, nil, int64(k)*10+i)
		}
		for ; nextA <= t; nextA += 0.032 {
			pkt(257, true, nil, append([]byte{0, 0, 1, 0xbd, 0, 0, 0x80, 0x80, 5}, cjTS(2, 1000+nextA+0.68+shift+pt.pes)...), int64(nextA*1000)+1e7)
		}
		for ; nextM <= t; nextM += 0.024 {
			pkt(258, true, nil, append([]byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0x80, 5}, cjTS(2, 1000+nextM+0.49+shift+pt.pes)...), int64(nextM*1000)+2e7)
		}
	}
	return out
}

// cjSeen: what a player is sent. ok: one clock throughout; repeats: pictures sent before.
type cjSeen struct {
	repeats, frames int
	bad             []string
}

func cjLook(b []byte) cjSeen {
	var s cjSeen
	healthy := map[int]float64{256: 0.65, 257: 0.68, 258: 0.49}
	last, lastCC := map[string]int64{}, map[int]int{}
	pcr := int64(-1)
	seen := map[[32]byte]bool{}
	var cur []byte
	end := func() {
		if cur != nil {
			s.frames++
			if h := sha256.Sum256(cur); seen[h] {
				s.repeats++
			} else {
				seen[h] = true
			}
		}
		cur = nil
	}
	step := func(k string, v int64, at int) {
		if p, ok := last[k]; ok && len(s.bad) < 6 {
			if d := float64(tsDiff(v, p)) / 90000; d < -0.06 || d > 0.2 {
				s.bad = append(s.bad, fmt.Sprintf("%s %+.3f s at byte %d", k, d, at))
			}
		}
		last[k] = v
	}
	for i := 0; i+188 <= len(b); i += 188 {
		p := b[i : i+188]
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		if c, ok := lastCC[pid]; ok && int(p[3]&15) != (c+1)&15 && len(s.bad) < 6 {
			s.bad = append(s.bad, fmt.Sprintf("continuity counter on %d at byte %d", pid, i))
		}
		lastCC[pid] = int(p[3] & 15)
		pos := 4
		if p[3]&0x20 != 0 {
			pos = 5 + int(p[4])
			pcr = int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7
			step("PCR", pcr, i)
		}
		if p[1]&0x40 == 0 {
			if pid == 256 && cur != nil {
				cur = append(cur, p[pos:]...)
			}
			continue
		}
		e := p[pos:]
		v := readTS(e[9:14])
		if e[7]>>6 == 3 {
			v = readTS(e[14:19])
		}
		step(fmt.Sprint("PES ", pid), v, i)
		if d := float64(tsDiff(v, pcr))/90000 - healthy[pid]; math.Abs(d) > 0.1 && len(s.bad) < 6 {
			s.bad = append(s.bad, fmt.Sprintf("PID %d is %+.3f s off its distance to the PCR at byte %d", pid, d, i))
		}
		if pid == 256 {
			end()
			cur = append([]byte{}, e[9+int(e[8]):]...)
		}
	}
	end()
	return s
}

// cjThrough sends b through clockFix in reads of lo to hi bytes. jumps: how often its offsets moved.
func cjThrough(b []byte, seed int64, lo, hi int) (out []byte, moves int) {
	r := rand.New(rand.NewSource(seed))
	var c clockFix
	sent, off, offPCR := int64(0), int64(0), int64(0)
	for i := 0; i < len(b); {
		n := min(lo+r.Intn(hi-lo+1), len(b)-i)
		got := c.fix(append([]byte(nil), b[i:i+n]...), sent)
		if c.off != off || c.offPCR != offPCR {
			moves++
			off, offPCR = c.off, c.offPCR
		}
		out = append(out, got...)
		sent += int64(len(got))
		i += n
	}
	return append(out, c.carry...), moves
}

// The provider starts the stream over some seconds back and sends that video again, on the same clock or on one
// that starts over: players are sent none of it, and the clock and continuity counters run on.
func TestClockJumpReplayLeftOut(t *testing.T) {
	for _, back := range []float64{4, 12, 40} {
		for _, restart := range []bool{false, true} {
			in := cjFeed{secs: 60, rewindAt: 50, rewind: back, restart: restart}.build()
			want := cjFeed{secs: 60}.build()
			if s := cjLook(in); s.repeats == 0 {
				t.Fatalf("%v s back: the stream sends nothing again", back)
			}
			for seed := int64(1); seed <= 8; seed++ {
				out, _ := cjThrough(in, seed, 1, 40000)
				s := cjLook(out)
				if s.repeats != 0 || len(s.bad) > 0 {
					t.Fatalf("%v s back (clock starts over: %v), seed %d: %d pictures sent again; %v", back, restart, seed, s.repeats, s.bad)
				}
				if w := cjLook(want).frames; s.frames < w-1 || s.frames > w+1 {
					t.Errorf("%v s back (clock starts over: %v), seed %d: %d pictures sent, a stream without the replay has %d", back, restart, seed, s.frames, w)
				}
			}
		}
	}
}

// An ad break whose clock jumps back a few seconds and then returns: the return, ahead by under 30 s, used to
// reach players as a leap.
func TestClockJumpAdBreakReturns(t *testing.T) {
	for _, j := range []float64{-5, -20, -600, 40} {
		in := cjFeed{secs: 30, parts: []cjPart{{}, {from: 10, pcr: j, pes: j}, {from: 20}}}.build()
		for seed := int64(1); seed <= 4; seed++ {
			out, moves := cjThrough(in, seed, 1, 40000)
			if s := cjLook(out); len(s.bad) > 0 || moves != 2 {
				t.Fatalf("ad break %+v s, seed %d: offsets moved %d times (want 2); %v", j, seed, moves, s.bad)
			}
		}
	}
}

// Only the PCR jumps (or only the PES timestamps): the other clock runs on untouched. With one offset for both,
// each jump of one moved the other off, and the two took turns, once a frame.
func TestClockJumpOneClockOnly(t *testing.T) {
	for _, part := range []cjPart{{from: 10, pcr: -600}, {from: 10, pcr: -10}, {from: 10, pes: -600}, {from: 10, pes: -10}} {
		in := cjFeed{secs: 30, parts: []cjPart{{}, part, {from: 20}}}.build()
		out, moves := cjThrough(in, 1, 1, 40000)
		if s := cjLook(out); len(s.bad) > 0 || moves > 4 { // (a jump of one moves the other's offset until it shows it ran on)
			t.Errorf("%+v: offsets moved %d times; %v", part, moves, s.bad)
		}
	}
}

// A source that loops a short clip, its clock starting over each pass: every pass is video sent before, but
// leaving it out would leave players with nothing. It is passed on.
func TestClockJumpLoopPassedOn(t *testing.T) {
	in := cjFeed{secs: 30, loop: 4}.build()
	out, _ := cjThrough(in, 1, 1316, 32768)
	if len(out) < len(in)*3/4 {
		t.Fatalf("a looping source: %d of %d bytes passed on", len(out), len(in))
	}
}

// Before any jump nothing is held or changed, whatever the reads.
func TestClockJumpNoneUntouched(t *testing.T) {
	in := cjFeed{secs: 20}.build()
	for seed := int64(1); seed <= 10; seed++ {
		if out, _ := cjThrough(in, seed, 1, 40000); string(out) != string(in) {
			t.Fatalf("seed %d: a stream whose clock never jumps was changed", seed)
		}
	}
}

// An ad splice that moves only the PCR (its PTS run on): one jump, and every PES keeps its distance to the PCR.
func TestClockJumpSplicePCROnly(t *testing.T) {
	in := cjFeed{secs: 20, parts: []cjPart{{}, {from: 10, pcr: -600}}}.build()
	var c clockFix
	var out []byte
	sent := int64(0)
	for i := 0; i < len(in); i += 1316 {
		got := c.fix(append([]byte(nil), in[i:min(len(in), i+1316)]...), sent)
		out, sent = append(out, got...), sent+int64(len(got))
	}
	out = append(out, c.carry...)
	if s := cjLook(out); c.jumps != 1 || len(s.bad) > 0 {
		t.Fatalf("%d jumps (want 1); %v", c.jumps, s.bad)
	}
}

// Pictures stop after a jump (only sound comes): what is held goes out within a second of stream.
func TestClockJumpHoldCapped(t *testing.T) {
	in := cjFeed{secs: 12, parts: []cjPart{{}, {from: 10, pcr: -600, pes: -600}}}.build()
	var c clockFix
	jumpAt := -1
	for i := 0; i+188 <= len(in); i += 188 { // (the first packet after the jump)
		if in[i+3]&0x20 != 0 && jumpAt < 0 {
			if v := int64(in[i+6])<<25 | int64(in[i+7])<<17 | int64(in[i+8])<<9 | int64(in[i+9])<<1; v < 900*90000 {
				jumpAt = i
			}
		}
	}
	var out []byte
	sent := int64(0)
	feed := func(b []byte) {
		got := c.fix(append([]byte(nil), b...), sent)
		out, sent = append(out, got...), sent+int64(len(got))
	}
	feed(in[:jumpAt+188])
	held := jumpAt + 188 - len(out)
	if held <= 0 {
		t.Fatalf("nothing held after the jump (jump at %d)", jumpAt)
	}
	var audio []byte // then only sound for 2 s
	for i := jumpAt + 188; i+188 <= len(in) && len(audio) < 188*60; i += 188 {
		if pid := int(in[i+1]&0x1f)<<8 | int(in[i+2]); pid == 257 {
			audio = append(audio, in[i:i+188]...)
		}
	}
	for k := 0; k+188 <= len(audio); k += 188 {
		feed(audio[k : k+188])
	}
	if got := len(out); got < jumpAt+188+len(audio)-188*2 {
		t.Fatalf("after 1 s of sound only, %d bytes still held", jumpAt+188+len(audio)-got)
	}
}
