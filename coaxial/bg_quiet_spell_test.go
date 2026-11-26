package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The hourly ping round is timed work the launcher does unasked, so it waits for a quiet spell. "Nothing plays
// right now" was enough before: a viewer whose player dropped out half a minute ago and was on its way back
// found every VPN server being pinged as it tuned again. The page's button still runs a round at once.
func TestPingRoundWaitsForAQuietSpell(t *testing.T) {
	fakePings(t, map[string]float64{"192.0.2.1": 4, "192.0.2.2": 30, "192.0.2.3": 25}, nil)
	var pings atomic.Int32
	answer := pingIP
	pingIP = func(ctx context.Context, ip string, count int) ([]time.Duration, int, error) {
		pings.Add(1)
		return answer(ctx, ip, count)
	}
	l := &launcher{dir: t.TempDir(), pickLoaded: true}
	fakeServers(t, l)
	l.pick = picked{Key: pickKey(l.set.vpnEnv()), Env: "SERVER_NAMES=NY#1", Name: "NY#1"} // (on the best one: nothing to move to)

	l.probeTick(30 * time.Second)
	if n := pings.Load(); n != 0 {
		t.Errorf("nothing has played for 30 s and %d servers were pinged: the quiet spell (%v) was not waited for", n, quietSpell)
	}
	l.probeTick(quietSpell)
	if pings.Load() == 0 {
		t.Errorf("control: nothing has played for a quiet spell and no server was pinged")
	}
	pings.Store(0)
	l.mu.Lock()
	l.probeAsked, l.probeAt = true, time.Now() // (the page's button, with no round due)
	l.mu.Unlock()
	l.probeTick(0)
	if pings.Load() == 0 {
		t.Errorf("the page asked for a round while something plays and no server was pinged")
	}
}
