package main

import "testing"

// The stream of a program table: the last one listed must not be missed.
func TestPickStream(t *testing.T) {
	table := func(types ...byte) []byte { // one entry per stream, none with descriptors
		n := 9 + 5*len(types) + 4
		s := []byte{0x02, 0xb0, byte(n), 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0}
		for i, typ := range types {
			s = append(s, typ, 0xe1, byte(i), 0xf0, 0)
		}
		return append(s, 0, 0, 0, 0)
	}
	for _, c := range []struct {
		types []byte
		want  int
	}{{[]byte{0x1b}, 0x100}, {[]byte{0x0f}, 0x100}, {[]byte{0x0f, 0x1b}, 0x101}, {[]byte{0x1b, 0x0f}, 0x100}, {[]byte{0x0f, 0x0f, 0x1b}, 0x102}} {
		if got := pickStream(table(c.types...)); got != c.want {
			t.Errorf("streams %v: got %#x, want %#x", c.types, got, c.want)
		}
	}
}
