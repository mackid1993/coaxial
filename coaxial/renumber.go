package main

import "slices"

// A provider asked for again may send the channel numbered afresh (another server): its program table and its
// streams on other PIDs. Players and recorders read a program's numbers when they start: ffmpeg, and the media
// servers and players built on it, took the new numbers for new streams and showed and recorded nothing more
// after such a splice. So a stream asked for again is handed on under the numbers the readers were given.

// numbering: a stream's numbers: its program table's PID, its clock's, and each stream's type and PID in order.
type numbering struct {
	pmt, pcr int
	es       [][2]int
}

// programOf: the numbering of the first program in buf (a PAT, then its PMT).
func programOf(buf []byte) (numbering, bool) {
	pmt := -1
	for i := cleanStart(buf); i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
		p, off := buf[i:i+188], 4
		if p[3]&0x20 != 0 {
			off = 5 + int(p[4])
		}
		if p[1]&0x40 == 0 || p[3]&0x10 == 0 || off >= 188 {
			continue
		}
		pid, s := int(p[1]&0x1f)<<8|int(p[2]), tableStart(p[off:])
		if pid == 0 {
			pmt = patPMT(s)
		} else if pid == pmt && len(s) >= 12 {
			pg := numbering{pmt: pmt, pcr: int(s[8]&0x1f)<<8 | int(s[9])}
			end := min(len(s)-4, 3+(int(s[1]&0x0f)<<8|int(s[2]))-4) // (minus the CRC)
			for k := 12 + (int(s[10]&0x0f)<<8 | int(s[11])); k+5 <= end; k += 5 + (int(s[k+3]&0x0f)<<8 | int(s[k+4])) {
				pg.es = append(pg.es, [2]int{int(s[k]), int(s[k+1]&0x1f)<<8 | int(s[k+2])})
			}
			return pg, len(pg.es) > 0
		}
	}
	return numbering{}, false
}

// renumber: a stream's PIDs put back to the readers' ones.
type renumber struct {
	to   map[int]int // its PID -> the readers'
	pmt  int         // its program table's PID
	part []byte      // the start of a packet not yet whole
}

// renumbering: from's numbers to to's; nil when they are the same, or nothing can be matched. Each stream
// goes to the readers' first one of the same type not yet taken (a provider's other server may list one
// more or one less, such as a second audio track, or list them in another order: matched in order, the
// whole stream was handed on under its own numbers, and recorders lost everything after it); one with no
// match keeps its PID. The clock follows its stream; on a PID of its own it goes to the readers' clock PID
// when theirs was one of its own too, else keeps its PID. (Put on the readers' video PID, its packets
// broke that PID's continuity counters hundreds of times a minute; asked to go two ways, nothing was
// renumbered.) No two of its PIDs end on one.
func renumbering(from, to numbering) *renumber {
	r, same, taken := &renumber{to: map[int]int{}, pmt: from.pmt}, true, map[int]bool{}
	set := func(a, b int) {
		r.to[a], taken[b], same = b, true, same && a == b
	}
	set(from.pmt, to.pmt)
	used := make([]bool, len(to.es))
	for _, e := range from.es {
		for k, f := range to.es {
			if !used[k] && f[0] == e[0] && r.to[e[1]] == 0 && !taken[f[1]] {
				used[k] = true
				set(e[1], f[1])
				break
			}
		}
	}
	if len(r.to) == 1 {
		return nil
	}
	if _, ok := r.to[from.pcr]; !ok && !taken[to.pcr] && !slices.ContainsFunc(to.es, func(f [2]int) bool { return f[1] == to.pcr }) {
		set(from.pcr, to.pcr)
	}
	for _, e := range append(from.es, [2]int{0, from.pcr}) { // (a PID kept must not be one given out)
		if _, ok := r.to[e[1]]; !ok && taken[e[1]] {
			return nil
		}
	}
	if same {
		return nil
	}
	return r
}

// apply: data under the readers' numbers, in whole packets (the rest waits for the next data).
func (r *renumber) apply(data []byte) []byte {
	b := append(r.part, data...)
	n := len(b) / 188 * 188
	r.part = append([]byte(nil), b[n:]...)
	for i := 0; i < n; i += 188 {
		p := b[i : i+188]
		if p[0] != 0x47 {
			continue
		}
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		if pid == 0 || pid == r.pmt {
			r.table(p, pid == 0)
		}
		if to, ok := r.to[pid]; ok {
			p[1], p[2] = p[1]&0xe0|byte(to>>8), byte(to)
		}
	}
	return b[:n]
}

// table: a PAT's or PMT's PIDs put back, and its CRC made again (a table that fits its packet, as they do).
func (r *renumber) table(p []byte, pat bool) {
	off := 4
	if p[3]&0x20 != 0 {
		off = 5 + int(p[4])
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || off >= 188 {
		return
	}
	s := tableStart(p[off:])
	if len(s) < 12 {
		return
	}
	n := 3 + (int(s[1]&0x0f)<<8 | int(s[2]))
	if n < 12 || n > len(s) {
		return
	}
	set := func(at int) {
		if to, ok := r.to[int(s[at]&0x1f)<<8|int(s[at+1])]; ok {
			s[at], s[at+1] = s[at]&0xe0|byte(to>>8), byte(to)
		}
	}
	if pat {
		for i := 8; i+4 <= n-4; i += 4 {
			if s[i] != 0 || s[i+1] != 0 { // (program 0 is the network table)
				set(i + 2)
			}
		}
	} else {
		set(8)
		for i := 12 + (int(s[10]&0x0f)<<8 | int(s[11])); i+5 <= n-4; i += 5 + (int(s[i+3]&0x0f)<<8 | int(s[i+4])) {
			set(i + 1)
		}
	}
	c := crcMPEG(s[:n-4])
	s[n-4], s[n-3], s[n-2], s[n-1] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
}

// crcMPEG: the CRC that ends a table (CRC-32/MPEG-2).
func crcMPEG(b []byte) uint32 {
	c := uint32(0xffffffff)
	for _, x := range b {
		c ^= uint32(x) << 24
		for k := 0; k < 8; k++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
	}
	return c
}
