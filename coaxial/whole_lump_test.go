package main

import (
	"bytes"
	"testing"
)

// Providers send live video in lumps seconds apart. Every whole packet of a lump is handed on as it arrives;
// only the start of a packet not yet whole waits for the next lump. (The lump's last packet was held back
// until the next lump came, six to twelve seconds later.)
func TestLumpEndsGoAtOnce(t *testing.T) {
	lump := splicePkts(0x100, 38, 50)
	got, rest := whole(append([]byte{}, lump...))
	if !bytes.Equal(got, lump) || len(rest) != 0 {
		t.Fatalf("a lump of whole packets: %d of %d bytes handed on, %d kept back", len(got), len(lump), len(rest))
	}
	got, rest = whole(append([]byte{}, lump[:len(lump)-100]...))
	if !bytes.Equal(got, lump[:len(lump)-188]) || !bytes.Equal(rest, lump[len(lump)-188:len(lump)-100]) {
		t.Fatalf("a lump ending inside a packet: %d bytes handed on, %d kept back", len(got), len(rest))
	}
}
