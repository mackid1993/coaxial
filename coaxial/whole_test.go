package main

import (
	"bytes"
	"testing"
)

// A provider hangs up anywhere, most often inside a packet, and may send padding before it does. Readers
// are handed whole packets only, and the stream asked for again carries on with the packet the old one
// stopped in: put together, exactly the provider's stream, not a byte of 0xff in it.
func TestStoppedMidPacketCarriesOnWhole(t *testing.T) {
	full := splicePkts(0x100, 38, 400) // one stream, a clock on every packet
	for k := 0; k < len(full); k += 188 {
		full[k+20] = byte(k / 188) // (packets told apart)
	}
	for _, tc := range []struct {
		name string
		cut  int    // bytes of the stream the old connection sent
		junk []byte // what it sent after them before it hung up
	}{
		{"a whole packet", 150 * 188, nil},
		{"inside a packet", 150*188 + 77, nil},
		{"4 bytes into a packet", 150*188 + 4, nil},
		{"inside a packet, then padding", 150*188 + 77, bytes.Repeat([]byte{0xff}, 100)},
		{"padding the size of two packets", 150 * 188, bytes.Repeat([]byte{0xff}, 376)},
	} {
		sent := append(append([]byte{}, full[:tc.cut]...), tc.junk...)
		var out, part []byte
		for i := 0; i < len(sent); i += 1000 { // (whatever the size of the reads)
			var d []byte
			d, part = whole(append(part, sent[i:min(i+1000, len(sent))]...))
			out = append(out, d...)
		}
		_, clock, at, ok := lastPCR(out, -1)
		if !ok {
			t.Fatalf("%s: no clock handed on", tc.name)
		}
		again := full[100*188:] // asked again: 2 s back
		from, ok := resumeAt(again, clock, len(out)-at, nil, nil)
		if !ok {
			t.Fatalf("%s: where it stopped was not found", tc.name)
		}
		if got := append(out, again[from:]...); !bytes.Equal(got, full) {
			n := 0
			for n < min(len(got), len(full)) && got[n] == full[n] {
				n++
			}
			t.Errorf("%s: readers' stream differs from the provider's at byte %d (packet %d)", tc.name, n, n/188)
		}
	}
}
