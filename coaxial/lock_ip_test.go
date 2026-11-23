package main

import (
	"net/http/httptest"
	"testing"
)

// A VPN reconnect the launcher reported starts the wait once. The status page's address look-up (at most every
// 5 minutes) finding the new address later must not start it again: by then streams play from it, and new ones
// would be turned away for nothing with "the VPN changed address a moment ago". A change nobody reported still
// starts it.
func TestAddressChangeWaitedForOnce(t *testing.T) {
	mu.Lock()
	wasIP, wasChanged := lastIP, vpnChanged
	mu.Unlock()
	defer func() { mu.Lock(); lastIP, vpnChanged = wasIP, wasChanged; mu.Unlock() }()
	sawAddress("198.51.100.1") // the status page was viewed
	insideStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/vpn-changed", nil))
	mu.Lock()
	vpnChanged = mono() - 200 // that wait is long over
	mu.Unlock()
	sawAddress("198.51.100.2") // the page is viewed again: the new address
	mu.Lock()
	left := lockLeft(true, 90, vpnChanged, mono())
	mu.Unlock()
	if left > 0 {
		t.Errorf("one VPN change was waited for twice: new streams wait %.0f s more, 200 s after it", left)
	}
	sawAddress("198.51.100.3") // a change the launcher did not see
	mu.Lock()
	left = lockLeft(true, 90, vpnChanged, mono())
	mu.Unlock()
	if left < 85 {
		t.Errorf("a new address seen only by the status page did not start the wait: %.0f s left", left)
	}
}
