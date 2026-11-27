package main

import (
	"strings"
	"testing"
)

// With no number of connections typed in, the refusal after a wait for a cut stream never says "All 0
// connections your provider allows are in use": there is no such number.
func TestRefusalWithNoLimitTypedNamesNoNumber(t *testing.T) {
	f := &front{name: "P", base: "http://198.51.100.20:80"}
	r := f.refusal("192.0.2.40", what{"vod", "9"}, "")
	if r == nil || strings.Contains(string(r.text), "All 0") || !strings.Contains(string(r.text), "has not closed") {
		t.Errorf("the refusal with no limit typed in reads %q", r.text)
	}
}
