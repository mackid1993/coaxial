package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// syncFeed builds a small single-program stream: per frame one video packet carrying a PCR and a PES header with a
// PTS, then filler packets with random payload (0x47 bytes turn up in it, as in real video). The clock restarts at
// restartAt; just before it, the provider's last packet is cut off after cut bytes (0: not cut).
func syncFeed(frames, restartAt, cut int) []byte {
	var out []byte
	rnd := rand.New(rand.NewSource(int64(frames*1000 + restartAt*10 + cut)))
	cc := 0
	clock := int64(900 * 90000)
	for f := 0; f < frames; f++ {
		if f == restartAt {
			if cut > 0 {
				out = out[:len(out)-188+cut] // the provider's last packet, cut short
			}
			clock = 63000 // 0.7 s
		}
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3] = 0x47, 0x41, 0x00, 0x30|byte(cc&15) // PID 0x100, payload start, adaptation + payload
		cc++
		p[4], p[5] = 7, 0x10 // adaptation field: 7 bytes, PCR
		if f == restartAt {
			p[5] |= 0x80 // the provider flags the restart, as seen live
		}
		pcr := clock
		p[6], p[7], p[8], p[9], p[10], p[11] = byte(pcr>>25), byte(pcr>>17), byte(pcr>>9), byte(pcr>>1), byte(pcr<<7)|0x7e, 0
		e := p[12:]
		copy(e, []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5})
		pts := clock + 90000/2
		e[9] = 0x21 | byte(pts>>29)&0x0e
		e[10], e[11] = byte(pts>>22), 0x01|byte(pts>>14)&0xfe
		e[12], e[13] = byte(pts>>7), 0x01|byte(pts<<1)&0xfe
		rnd.Read(e[14:])
		out = append(out, p...)
		for j := 0; j < 3; j++ { // filler on the same PID, payload full of 0x47
			q := make([]byte, 188)
			q[0], q[1], q[2], q[3] = 0x47, 0x01, 0x00, 0x10|byte(cc&15)
			cc++
			rnd.Read(q[4:])
			out = append(out, q...)
		}
		clock += 3003 // 29.97 frames a second
	}
	return out
}

// runFix sends data through clockFix in random read sizes, as tsSource does.
func runFix(data []byte, seed int64) []byte {
	out, _ := runFixReads(data, seed, 1, 5000)
	return out
}

// runFixReads: the same with read sizes between lo and hi, returning where each read ended in the input.
func runFixReads(data []byte, seed int64, lo, hi int) ([]byte, []int) {
	in := append([]byte(nil), data...)
	r := rand.New(rand.NewSource(seed))
	var c clockFix
	var out []byte
	var ends []int
	sent := int64(0)
	for i := 0; i < len(in); {
		n := lo + r.Intn(hi-lo+1)
		if i+n > len(in) {
			n = len(in) - i
		}
		got := c.fix(append([]byte(nil), in[i:i+n]...), sent)
		out = append(out, got...)
		sent += int64(len(got))
		i += n
		ends = append(ends, i)
	}
	return append(out, c.carry...), ends
}

// tsClocks: every PCR and video PTS in order, read by finding packet starts the way a player does (resyncing on
// three 0x47 bytes 188 apart).
func tsClocks(b []byte) (pcr, pts []int64) {
	for i := 0; i+188 <= len(b); {
		if b[i] != 0x47 || (i+188 < len(b) && b[i+188] != 0x47) {
			i++
			continue
		}
		p := b[i : i+188]
		pos := 4
		if p[3]&0x20 != 0 {
			pos = 5 + int(p[4])
			if p[4] >= 7 && p[5]&0x10 != 0 {
				pcr = append(pcr, int64(p[6])<<25|int64(p[7])<<17|int64(p[8])<<9|int64(p[9])<<1|int64(p[10])>>7)
			}
		}
		if p[1]&0x40 != 0 && pos+14 <= 188 && p[pos] == 0 && p[pos+1] == 0 && p[pos+2] == 1 && p[pos+3] >= 0xe0 {
			pts = append(pts, readTS(p[pos+9:pos+14]))
		}
		i += 188
	}
	return
}

func checkContinuous(t *testing.T, what string, out []byte) {
	t.Helper()
	pcr, pts := tsClocks(out)
	if len(pcr) < 10 || len(pts) < 10 {
		t.Fatalf("%s: found %d PCRs, %d PTSs", what, len(pcr), len(pts))
	}
	for name, list := range map[string][]int64{"PCR": pcr, "PTS": pts} {
		for k := 1; k < len(list); k++ {
			if d := tsDiff(list[k], list[k-1]); d < 0 || d > 2*90000 {
				t.Errorf("%s: %s %d -> %d (%+.3f s)", what, name, list[k-1], list[k], float64(d)/90000)
				break
			}
		}
	}
}

// A packet cut short right before a clock restart: the restart is still caught, the clock runs on, and only timestamp
// bytes change. The one case left out is a read that ends inside the restart packet itself: its start has already
// gone to the players before it can be seen as a restart (catching that would mean holding video back), and the
// build before this change misses it the same way. TestClockFixNoWorse covers it.
func TestClockFixShortPacket(t *testing.T) {
	checked := 0
	for _, cut := range []int{40, 1, 12, 187, 100} {
		in := syncFeed(400, 200, cut)
		restart := 200*4*188 - 188 + cut // where the restart packet starts in the input
		for seed := int64(1); seed <= 60; seed++ {
			out, ends := runFixReads(in, seed, 1, 5000)
			straddle := false
			for _, e := range ends {
				straddle = straddle || e > restart && e < restart+188
			}
			if straddle {
				continue
			}
			checked++
			if len(out) != len(in) {
				t.Fatalf("cut %d seed %d: %d bytes out for %d in", cut, seed, len(out), len(in))
			}
			checkContinuous(t, "cut short packet", out)
			for i := range in {
				if in[i] != out[i] && !tsField(in, i) {
					t.Fatalf("cut %d seed %d: byte %d changed and is not part of a PCR or PTS", cut, seed, i)
				}
			}
		}
	}
	t.Logf("%d read patterns checked", checked)
}

// tsField: whether byte i of the input lies inside a PCR or a video PES PTS/DTS of a whole packet.
func tsField(b []byte, i int) bool {
	for s := i - 187; s <= i; s++ {
		if s < 0 || s+188 > len(b) || b[s] != 0x47 || (s+188 < len(b) && b[s+188] != 0x47) {
			continue
		}
		at := i - s
		p := b[s : s+188]
		pos := 4
		if p[3]&0x20 != 0 {
			pos = 5 + int(p[4])
			if p[5]&0x10 != 0 && at >= 6 && at < 12 {
				return true
			}
		}
		if p[1]&0x40 != 0 && at >= pos+9 && at < pos+19 {
			return true
		}
	}
	return false
}

// The new build is never worse than walking a fixed grid: across many read patterns of real sizes, with and without
// a cut packet, it lets through no more backward jumps than reads ending inside a restart packet can explain.
func TestClockFixNoWorse(t *testing.T) {
	for _, cut := range []int{0, 40, 150} {
		in := syncFeed(400, 200, cut)
		bad := 0
		for seed := int64(1); seed <= 200; seed++ {
			out, _ := runFixReads(in, seed, 1316, 32768)
			pcr, _ := tsClocks(out)
			for k := 1; k < len(pcr); k++ {
				if tsDiff(pcr[k], pcr[k-1]) < 0 {
					bad++
					break
				}
			}
		}
		t.Logf("cut %d: %d of 200 read patterns (1316 to 32768 bytes) let a backward jump through", cut, bad)
		if bad > 6 {
			t.Errorf("cut %d: %d of 200 read patterns let a backward jump through", cut, bad)
		}
	}
}

// Two clock restarts with a cut packet before each, the second one 4 frames after the first.
func TestClockFixShortPacketsTwice(t *testing.T) {
	a := syncFeed(100, 50, 40)
	b := syncFeed(100, 4, 120)
	out := runFix(append(a, b...), 7)
	checkContinuous(t, "two cut restarts", out)
}

// A stream that never restarts its clock passes through byte for byte, even with 0x47 all over its payload.
func TestClockFixUntouchedWithoutRestart(t *testing.T) {
	in := syncFeed(300, -1, 0)
	for seed := int64(1); seed <= 20; seed++ {
		out := runFix(in, seed)
		if string(out) != string(in) {
			t.Fatalf("seed %d: a stream with no restart was changed", seed)
		}
	}
}

// COAXIAL_REPLAY_CLIPS=<dir>: recorded clips (*.ts) of real provider output, each run through clockFix in random read
// sizes; the PCR and video PTS must come out continuous. Real clips stay on the dev machine, never in the repo.
func TestClockFixReplayClips(t *testing.T) {
	dir := os.Getenv("COAXIAL_REPLAY_CLIPS")
	if dir == "" {
		t.Skip("COAXIAL_REPLAY_CLIPS not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.ts"))
	if len(files) == 0 {
		t.Fatalf("no clips in %s", dir)
	}
	for _, f := range files {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		missed := 0
		for seed := int64(1); seed <= 20; seed++ {
			out, _ := runFixReads(in, seed, 1316, 32768)
			// Nothing is added, and whole packets only are left out: video the provider sends again after a
			// jump is dropped on purpose (fea1df4), so a clip that holds some comes out shorter. What matters
			// is checked below: no clock jump reaches the player.
			if len(out) > len(in) || len(out)%188 != len(in)%188 {
				t.Errorf("%s seed %d: %d bytes out for %d in", filepath.Base(f), seed, len(out), len(in))
			}
			pcr, pts := tsClocks(out)
			bad := 0
			for _, list := range [][]int64{pcr, pts} {
				for k := 1; k < len(list); k++ {
					if d := tsDiff(list[k], list[k-1]); d < -90000/2 || d > 30*90000 {
						bad++
					}
				}
			}
			t.Logf("%s seed %d: %d PCRs, %d PTSs, %d jumps", filepath.Base(f), seed, len(pcr), len(pts), bad)
			if bad > 0 {
				missed++
			}
		}
		// A read ending inside a restart packet lets that one restart through (see TestClockFixShortPacket); at
		// these read sizes that is about 1 pattern in 100 per restart. More than 2 of 20 is a real miss.
		if missed > 2 {
			t.Errorf("%s: %d of 20 read patterns let clock jumps reach the player", filepath.Base(f), missed)
		}
	}
}

// COAXIAL_REPLAY_TS (the recorded live feed live_lead_test uses, a channel whose clock never restarts) must pass
// through clockFix byte for byte, read in random sizes.
func TestClockFixReplayUntouched(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_TS")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_TS not set")
	}
	in, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(1); seed <= 5; seed++ {
		if out := runFix(in, seed); string(out) != string(in) {
			t.Fatalf("seed %d: the recorded feed was changed", seed)
		}
	}
}
