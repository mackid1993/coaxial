package main

import "testing"

// A value that cannot be passed to a program (a NUL in it) is refused, as it is under "extra".
func TestVarsNul(t *testing.T) {
	s := goodSettings()
	s.VPN.Vars["server_cities"] = "New York\x00"
	if errs := s.validate(); len(errs) != 1 || errs[0].Field != "vpn.server_cities" {
		t.Errorf("got %v", errs)
	}
}
