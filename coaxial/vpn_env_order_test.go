package main

import "testing"

// Two keys under "vpn" that name the same gluetun variable ("type" and "vpn_type") give the same answer
// every time, so the VPN is not started again, or with the other value, at random.
func TestVPNEnvSameEveryTime(t *testing.T) {
	s := goodSettings()
	s.VPN.Vars["vpn_service_provider"] = "protonvpn"
	first := s.vpnEnv()["VPN_SERVICE_PROVIDER"]
	for i := 0; i < 200; i++ {
		if got := s.vpnEnv()["VPN_SERVICE_PROVIDER"]; got != first {
			t.Fatalf("VPN_SERVICE_PROVIDER was %q, then %q", first, got)
		}
	}
}
