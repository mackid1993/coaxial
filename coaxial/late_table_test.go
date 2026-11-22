package main

import "testing"

// A stream asked for again, numbered afresh, may send its program table seconds after its video. Its numbers
// are put back once the table has come; joined before, every packet went on under the new numbers.
func TestRenumberedStreamWaitsForItsTable(t *testing.T) {
	pub, _ := programOf(numbered(0x1000, 0x100, 0x101))
	untabled := func(b []byte) []byte { // the same stream with its tables (PAT, PMT) not yet sent
		var out []byte
		for i := 0; i < len(b); i += 188 {
			if pid := int(b[i+1]&0x1f)<<8 | int(b[i+2]); pid != 0 && pid != 0x1000 && pid != 0x1100 {
				out = append(out, b[i:i+188]...)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"numbered afresh, no table yet", untabled(numbered(0x1100, 0x200, 0x201)), true},
		{"numbered afresh, its table came", numbered(0x1100, 0x200, 0x201), false},
		{"numbered as before, no table yet", untabled(numbered(0x1000, 0x100, 0x101)), false},
	} {
		if got := programDue(tc.b, 0, pub); got != tc.want {
			t.Errorf("%s: waits for its table %v, want %v", tc.name, got, tc.want)
		}
	}
	if programDue(untabled(numbered(0x1100, 0x200, 0x201)), 0, numbering{}) {
		t.Error("waited although the readers were given no numbers")
	}
	long := untabled(numbered(0x1100, 0x200, 0x201)) // (its clock: 40 s, then 46 s on)
	long = append(append(long, splicePkts(0x200, 40, 1)...), splicePkts(0x200, 46, 1)...)
	if programDue(long, 0, pub) {
		t.Error("still waiting for its table after more than keyWait seconds of the stream")
	}
}
