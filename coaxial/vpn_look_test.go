package main

import (
	"testing"
	"time"
)

// While the VPN is up, one look at it that fails is taken again before the VPN is called down. A single failed
// look (the health check is a program started for it, with 5 s to answer) used to be enough: the log said the
// VPN was down and, two seconds later, up again, with every stream still running.
func TestOneFailedLookIsNotDown(t *testing.T) {
	old := lookAgain
	lookAgain = 10 * time.Millisecond
	defer func() { lookAgain = old }()
	c := &child{}
	l := &launcher{vpn: c, vpnUp: true}
	looks := func(answers ...bool) (func() bool, *int) {
		n := 0
		return func() bool {
			n++
			return answers[min(n, len(answers))-1]
		}, &n
	}

	look, n := looks(false, true)
	if up := l.looksUp(c, look); !up || *n != 2 {
		t.Errorf("one failed look, then a good one: up %v after %d look(s), want up after 2", up, *n)
	}
	look, n = looks(false, false)
	if up := l.looksUp(c, look); up || *n != 2 {
		t.Errorf("two failed looks: up %v after %d look(s), want down after 2", up, *n)
	}
	look, n = looks(true)
	if up := l.looksUp(c, look); !up || *n != 1 {
		t.Errorf("a good look: up %v after %d look(s), want up after 1", up, *n)
	}
	l.vpnUp = false // (not up yet, as at a start: a failed look is "not yet", and nothing waits for a second one)
	look, n = looks(false, true)
	if up := l.looksUp(c, look); up || *n != 1 {
		t.Errorf("not up yet and a failed look: up %v after %d look(s), want not up after 1", up, *n)
	}
}
