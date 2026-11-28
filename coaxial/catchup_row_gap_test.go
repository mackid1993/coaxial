package main

import "testing"

// A player reads a catch-up's pieces ahead in bursts, about every 30 s and at times 31 s apart. The catch-up
// stays one row, counted as playing, across such a gap: it was ended and started again every minute.
func TestCatchUpRowOutlastsAReadAheadGap(t *testing.T) {
	mu.Lock()
	defer mu.Unlock()
	old := hlsRows
	hlsRows = map[string]*hlsRow{}
	defer func() { hlsRows = old }()
	f := &front{base: "http://provider.example", port: "8095", name: "P"}
	f.catchUpRead("192.0.2.9", "77")
	rk := "192.0.2.9 http://provider.example 77 catch-up"
	row := hlsRows[rk]
	if row == nil {
		t.Fatal("no row for the catch-up")
	}
	row.last -= 45 // (the next burst is 45 s after the last piece)
	forgetIdle()
	if hlsRows[rk] != row {
		t.Fatal("a 45 s gap between bursts ended the catch-up's row")
	}
	f.catchUpRead("192.0.2.9", "77")
	if hlsRows[rk] != row {
		t.Fatal("the next burst after a 45 s gap started a new row")
	}
	live := &hlsRow{last: mono() - 40}
	if live.keep() != 30 {
		t.Errorf("a live channel's row keeps %v s, want 30", live.keep())
	}
}
