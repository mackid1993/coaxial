package main

import (
	"slices"
	"testing"
)

// tablePkt: a PAT (pid 0) or PMT packet with a good CRC.
func tablePkt(pid int, section []byte) []byte {
	c := crcMPEG(section)
	return cutPkt(pid, true, nil, append(append([]byte{0}, section...), byte(c>>24), byte(c>>16), byte(c>>8), byte(c)))
}

// numbered: a stream whose program table is on pmt, its video on v (its clock too) and audio on a.
func numbered(pmt, v, a int) []byte {
	pat := tablePkt(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | byte(pmt>>8), byte(pmt)})
	pmtp := tablePkt(pmt, []byte{0x02, 0xb0, 23, 0, 1, 0xc1, 0, 0, 0xe0 | byte(v>>8), byte(v), 0xf0, 0,
		0x1b, 0xe0 | byte(v>>8), byte(v), 0xf0, 0, 0x0f, 0xe0 | byte(a>>8), byte(a), 0xf0, 0})
	out := append(append([]byte{}, pat...), pmtp...)
	for k := 0; k < 20; k++ {
		out = append(out, cutPkt(v, k == 0, []byte{0x40}, []byte{0, 0, 1, 0xe0})...)
		out = append(out, cutPkt(a, k == 0, nil, []byte{0, 0, 1, 0xc0})...)
	}
	return out
}

// A stream asked for again that numbers its streams afresh is handed on under the numbers the readers have,
// its tables rewritten with good CRCs, whatever the size of the reads.
func TestRenumberedStreamKeepsReadersNumbers(t *testing.T) {
	old, _ := programOf(numbered(0x1000, 0x100, 0x101))
	again := numbered(0x1100, 0x200, 0x201)
	p, ok := programOf(again)
	if !ok || p.pmt != 0x1100 || len(p.es) != 2 {
		t.Fatalf("numbering of the stream asked again: %+v %v", p, ok)
	}
	rn := renumbering(p, old)
	if rn == nil {
		t.Fatal("not renumbered")
	}
	var out []byte
	for i := 0; i < len(again); i += 1000 {
		out = append(out, rn.apply(append([]byte{}, again[i:min(i+1000, len(again))]...))...)
	}
	if len(out) != len(again) {
		t.Fatalf("%d bytes handed on of %d", len(out), len(again))
	}
	if got, ok := programOf(out); !ok || got.pmt != old.pmt || got.pcr != old.pcr || got.es[0] != old.es[0] || got.es[1] != old.es[1] {
		t.Errorf("readers see %+v, want %+v", got, old)
	}
	for i := 0; i < len(out); i += 188 {
		pid := int(out[i+1]&0x1f)<<8 | int(out[i+2])
		if pid != 0 && pid != 0x1000 && pid != 0x100 && pid != 0x101 {
			t.Fatalf("packet %d on PID %#x", i/188, pid)
		}
		if pid == 0 || pid == 0x1000 {
			s := tableStart(out[i+4:])
			n := 3 + (int(s[1]&0x0f)<<8 | int(s[2]))
			if c := crcMPEG(s[:n-4]); s[n-4] != byte(c>>24) || s[n-1] != byte(c) {
				t.Errorf("table on PID %#x: bad CRC", pid)
			}
		}
	}
	if renumbering(old, old) != nil {
		t.Error("a stream numbered as before was renumbered")
	}
}

// progPkts: a stream whose program table is on pmt and lists es (type, PID), its clock on pcr (on a PID of
// its own: clock-only packets there), a few packets of each stream.
func progPkts(pmt, pcr int, es ...[2]int) []byte {
	pat := tablePkt(0, []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | byte(pmt>>8), byte(pmt)})
	sec := []byte{0x02, 0xb0, 0, 0, 1, 0xc1, 0, 0, 0xe0 | byte(pcr>>8), byte(pcr), 0xf0, 0}
	for _, e := range es {
		sec = append(sec, byte(e[0]), 0xe0|byte(e[1]>>8), byte(e[1]), 0xf0, 0)
	}
	sec[2] = byte(len(sec) - 3 + 4)
	out := append(append([]byte{}, pat...), tablePkt(pmt, sec)...)
	for k := 0; k < 20; k++ {
		if pcr != es[0][1] { // a clock-only packet (an adaptation field and no payload)
			p := splicePkts(pcr, 40+0.04*float64(k), 1)
			p[3], p[4] = 0x20, 183
			out = append(out, p...)
		}
		for _, e := range es {
			out = append(out, cutPkt(e[1], k == 0, nil, []byte{0, 0, 1, 0xe0})...)
		}
	}
	return out
}

// renumbered: what readers are handed of stream again, which readers saw numbered as first; and whether
// any two of its PIDs ended on one.
func renumbered(t *testing.T, first, again []byte) (numbering, []byte) {
	t.Helper()
	pub, _ := programOf(first)
	p, _ := programOf(again)
	rn := renumbering(p, pub)
	if rn == nil {
		return p, again
	}
	onto := map[int]int{}
	for a, b := range rn.to {
		if c, ok := onto[b]; ok {
			t.Errorf("PIDs %#x and %#x both handed on as %#x", a, c, b)
		}
		onto[b] = a
	}
	out := rn.apply(append([]byte{}, again...))
	got, _ := programOf(out)
	return got, out
}

// A provider's other server may carry the clock on a PID of its own where the first carried it on the
// video, or the other way round. Its clock-only packets are not handed on as video (they broke the video's
// continuity counters), and the stream is still handed on under the readers' numbers.
func TestRenumberClockOnItsOwnPID(t *testing.T) {
	v, a := [2]int{0x1b, 0x100}, [2]int{0x0f, 0x101}
	got, out := renumbered(t, progPkts(0x1000, 0x100, v, a), progPkts(0x1100, 0x1ff0, [2]int{0x1b, 0x200}, [2]int{0x0f, 0x201}))
	if got.pmt != 0x1000 || got.es[0] != v || got.es[1] != a || got.pcr == 0x100 || got.pcr == 0x101 {
		t.Errorf("clock moved to a PID of its own: readers see %+v", got)
	}
	for i := 0; i < len(out); i += 188 {
		if pid := int(out[i+1]&0x1f)<<8 | int(out[i+2]); out[i+3]&0x10 == 0 && pid != got.pcr {
			t.Fatalf("a clock-only packet handed on as PID %#x, its table says %#x", pid, got.pcr)
		}
	}
	got, _ = renumbered(t, progPkts(0x1000, 0x1ff, v, a), progPkts(0x1100, 0x200, [2]int{0x1b, 0x200}, [2]int{0x0f, 0x201}))
	if got.pmt != 0x1000 || got.es[0] != v || got.es[1] != a || got.pcr != 0x100 {
		t.Errorf("clock moved onto the video: readers see %+v", got)
	}
}

// A provider's other server may list one stream more or one less (a second audio track, no audio) or list
// them in another order: each stream is still handed on under the readers' PID for its kind.
func TestRenumberOtherStreams(t *testing.T) {
	v, a := [2]int{0x1b, 0x100}, [2]int{0x0f, 0x101}
	first := progPkts(0x1000, 0x100, v, a)
	for _, tc := range []struct {
		name string
		es   [][2]int
		want [][2]int
	}{
		{"a second audio track", [][2]int{{0x1b, 0x200}, {0x0f, 0x201}, {0x0f, 0x202}}, [][2]int{v, a, {0x0f, 0x202}}},
		{"no audio", [][2]int{{0x1b, 0x200}}, [][2]int{v}},
		{"audio listed first", [][2]int{{0x0f, 0x201}, {0x1b, 0x200}}, [][2]int{a, v}},
	} {
		got, _ := renumbered(t, first, progPkts(0x1100, tc.es[0][1], tc.es...))
		if got.pmt != 0x1000 || !slices.Equal(got.es, tc.want) {
			t.Errorf("%s: readers see %+v, want %v on PMT 0x1000", tc.name, got, tc.want)
		}
	}
}
