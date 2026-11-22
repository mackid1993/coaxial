package main

import "testing"

// Asked again, a provider may send the same pictures encoded afresh (its encoder started again) on the same
// clock, the sound passed through unchanged: the packets of sound are the same, those of video are not. It is
// not taken to carry on after the last packet of sound passed on, which went on into the other encoding
// mid-picture (broken frames up to its next keyframe); spliceAt joins it at a keyframe.
func TestResumeNotIntoAnotherEncoding(t *testing.T) {
	mux := func(enc byte) []byte { // video on 0x100 (a clock on every fifth), sound on 0x101 after every fourth
		var out []byte
		for k := 0; k < 400; k++ {
			p := splicePkts(0x100, 38+0.04*float64(k/5), 1)
			if k%5 != 0 {
				p[3] = 0x10
				copy(p[4:], make([]byte, 8))
			}
			p[30], p[31], p[32] = byte(k>>8), byte(k), enc // (its own picture data)
			out = append(out, p...)
			if k%4 == 3 {
				s := cutPkt(0x101, false, nil, nil)
				s[30], s[31] = byte(k>>8), byte(k)
				out = append(out, s...)
			}
		}
		return out
	}
	first, again := mux(1), mux(2)
	cut := 0
	for i := 0; i < len(first); i += 188 { // the old stream ends with a packet of sound
		if first[i+2] == 0x01 && i > len(first)/2 {
			cut = i + 188
			break
		}
	}
	old := first[:cut]
	_, clock, at, _ := lastPCR(old, -1)
	var video []byte
	for i := len(old) - 188; video == nil; i -= 188 {
		if old[i+2] == 0x00 {
			video = old[i : i+188]
		}
	}
	if from, ok := resumeAt(again[100*188:], clock, len(old)-at, old[len(old)-188:], video); ok && from <= len(again)-100*188 {
		t.Errorf("carried on into another encoding at packet %d", from/188)
	}
	if from, ok := resumeAt(first[100*188:], clock, len(old)-at, old[len(old)-188:], video); !ok || from != cut-100*188 {
		t.Errorf("the same stream: carries on at packet %d (%v), want %d", from/188, ok, (cut-100*188)/188)
	}
}

// Two programs made from one source on one clock: their packets of sound are the same, and each carries
// the clock. The stream asked for again carries on just after the last packet passed on, not after the
// other program's copy of it (packets sent twice), whichever program's clock packet comes first.
func TestResumeTwoProgramsOneClock(t *testing.T) {
	var full []byte // per step: program 2's video (clock), its sound, program 1's video (clock), its sound
	for k := 0; k < 300; k++ {
		for _, pid := range []int{0x102, 0x103, 0x100, 0x101} {
			p := splicePkts(pid, 38+0.04*float64(k), 1)
			if pid&1 == 1 { // sound: the same in both programs
				p = cutPkt(pid, false, nil, nil)
				p[30], p[31] = byte(k>>8), byte(k)
			} else {
				p[30], p[31], p[32] = byte(k>>8), byte(k), byte(pid)
			}
			full = append(full, p...)
		}
	}
	cut := 200 * 4 * 188 // (after program 1's sound)
	old := full[:cut]
	_, clock, at, _ := lastPCR(old, 0x100)
	from, ok := resumeAt(full[100*4*188:], clock, len(old)-at, old[len(old)-188:], old[len(old)-376:len(old)-188])
	if want := cut - 100*4*188; !ok || from != want {
		t.Errorf("carries on at packet %d (%v), want %d", from/188, ok, want/188)
	}
}
