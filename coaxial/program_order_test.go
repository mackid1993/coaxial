package main

import "testing"

// twoPrograms: a stream with two programs (tables on 0x1000 and 0x1001, streams 0x100/0x101 and 0x102/0x103),
// its PAT listing them in the order given.
func twoPrograms(order ...int) []byte {
	pat := []byte{0x00, 0xb0, 17, 0, 1, 0xc1, 0, 0}
	for _, n := range order {
		pat = append(pat, 0, byte(n), 0xf0, byte(n-1))
	}
	out := tablePkt(0, pat)
	for n := 1; n <= 2; n++ {
		v, a := 0x100+2*(n-1), 0x101+2*(n-1)
		out = append(out, tablePkt(0x1000+n-1, []byte{0x02, 0xb0, 23, 0, byte(n), 0xc1, 0, 0, 0xe0 | byte(v>>8), byte(v), 0xf0, 0,
			0x1b, 0xe0 | byte(v>>8), byte(v), 0xf0, 0, 0x0f, 0xe0 | byte(a>>8), byte(a), 0xf0, 0})...)
	}
	for k := 0; k < 20; k++ {
		for _, pid := range []int{0x100, 0x101, 0x102, 0x103} {
			out = append(out, cutPkt(pid, k == 0, nil, []byte{0, 0, 1, 0xe0})...)
		}
	}
	return out
}

// Asked again, a stream with two programs may list them the other way round (another server). Its first
// program is then the other one: it is not "numbered afresh" onto the readers' numbers, which put both
// programs' video on one PID and both their sound on another, and broke both.
func TestTwoProgramsListedTheOtherWay(t *testing.T) {
	pub, _ := programOf(twoPrograms(1, 2))
	again := twoPrograms(2, 1)
	p, ok := programOf(again)
	if !ok {
		t.Fatal("no program in the stream asked again")
	}
	rn := renumbering(p, pub)
	if rn == nil {
		return
	}
	out := rn.apply(append([]byte{}, again...))
	from := map[int]int{} // a PID handed on -> the stream's PID it came from
	for i := 0; i < len(out); i += 188 {
		pid, was := int(out[i+1]&0x1f)<<8|int(out[i+2]), int(again[i+1]&0x1f)<<8|int(again[i+2])
		if f, seen := from[pid]; seen && f != was {
			t.Fatalf("PIDs %#x and %#x both handed on as %#x", f, was, pid)
		}
		from[pid] = was
	}
}
