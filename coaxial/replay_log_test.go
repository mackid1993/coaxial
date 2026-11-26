package main

import (
	"math"
	"regexp"
	"strconv"
	"testing"
)

// The line for a clock jump says how long the video left out took to come (left_out_took_s), next to how much
// video it was (left_out_s): a stream started over sends it in a burst, far faster than it plays. No recording
// of a real one holds the times of its reads, so the log is where that is seen.
func TestClockJumpLogSaysHowLongTheReplayTook(t *testing.T) {
	in := cjFeed{secs: 60, rewindAt: 50, rewind: 12, restart: true}.build()
	log := captureLog(t)
	var c clockFix
	now := 0.0
	c.now = func() float64 { return now }
	sent, out := int64(0), 0
	for i := 0; i < len(in); i += 18800 { // a read every 10 ms
		got := c.fix(append([]byte(nil), in[i:min(len(in), i+18800)]...), sent)
		sent, out = sent+int64(len(got)), out+len(got)
		now += 0.01
	}
	lines := linesWith(log.String(), "a channel's clock jumped", "which follows on from the last one passed on")
	if len(lines) != 1 || len(in)-out == 0 {
		t.Fatalf("%d lines for a jump with video left out (%d bytes were), want 1:\n%s", len(lines), len(in)-out, log.String())
	}
	m := regexp.MustCompile(`left_out_took_s=([0-9.]+)`).FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("the line does not say how long the video left out took to come: %s", lines[0])
	}
	took, _ := strconv.ParseFloat(m[1], 64)
	if want := float64(len(in)-out) / 18800 * 0.01; math.Abs(took-want) > 0.2 {
		t.Errorf("left_out_took_s=%v, but the %d bytes left out came in %.1f s", took, len(in)-out, want)
	}
}
