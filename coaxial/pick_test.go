package main

import "testing"

func TestPickKey(t *testing.T) {
	base := map[string]string{"VPN_SERVICE_PROVIDER": "protonvpn", "VPN_TYPE": "wireguard", "SERVER_CITIES": "New York"}
	if pickKey(base) == "" {
		t.Errorf("a place and no server: a server is to be picked")
	}
	for name, change := range map[string][2]string{
		"a server named":      {"SERVER_NAMES", "US-NY#1"},
		"a hostname named":    {"SERVER_HOSTNAMES", "a.example"},
		"the user's own file": {"VPN_SERVICE_PROVIDER", "custom"},
		"no city":             {"SERVER_CITIES", ""},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[change[0]] = change[1]
		if pickKey(env) != "" {
			t.Errorf("%s: no server is to be picked, got %q", name, pickKey(env))
		}
	}
	if pickKey(map[string]string{"VPN_SERVICE_PROVIDER": "protonvpn", "SERVER_COUNTRIES": "United States"}) != "" {
		t.Errorf("a country alone is too many servers to try: no server is to be picked")
	}
	with := map[string]string{"VPN_SERVICE_PROVIDER": "protonvpn", "VPN_TYPE": "wireguard", "SERVER_CITIES": "New York", "STREAM_ONLY": "on"}
	if pickKey(with) == pickKey(base) {
		t.Errorf("a changed ..._ONLY setting must call for a new choice")
	}
}

func TestCandidates(t *testing.T) {
	list := []serverEntry{
		{VPN: "wireguard", Country: "United States", City: "New York", ServerName: "NY#1", Hostname: "a"},
		{VPN: "openvpn", Country: "United States", City: "New York", ServerName: "NY#1", Hostname: "a"},
		{VPN: "wireguard", Country: "United States", City: "New York", ServerName: "NY#2", Hostname: "b", Stream: true},
		{VPN: "wireguard", Country: "United States", City: "New York", ServerName: "CH-US#1", Hostname: "c", SecureCore: true},
		{VPN: "wireguard", Country: "United States", City: "New York", ServerName: "NY-TOR#1", Hostname: "d", Tor: true},
		{VPN: "wireguard", Country: "United States", City: "New York", ServerName: "NY-FREE#1", Hostname: "e", Free: true},
		{VPN: "wireguard", Country: "United States", City: "Chicago", ServerName: "CHI#1", Hostname: "f"},
		{VPN: "wireguard", Country: "Sweden", City: "Malmo", Hostname: "se-mma-wg-001"},
	}
	names := func(env map[string]string) string {
		s := ""
		for _, c := range candidatesFrom(list, env) {
			s += c.env + " "
		}
		return s
	}
	ny := map[string]string{"VPN_TYPE": "wireguard", "SERVER_COUNTRIES": "united states", "SERVER_CITIES": "New York"}
	if got := names(ny); got != "SERVER_NAMES=NY#1 SERVER_NAMES=NY#2 " {
		t.Errorf("plain New York: %q (no secure core, Tor or free server unless asked for; one entry per server)", got)
	}
	ny["STREAM_ONLY"] = "on"
	if got := names(ny); got != "SERVER_NAMES=NY#2 " {
		t.Errorf("stream only: %q", got)
	}
	delete(ny, "STREAM_ONLY")
	ny["SECURE_CORE_ONLY"] = "on"
	if got := names(ny); got != "SERVER_NAMES=CH-US#1 " {
		t.Errorf("secure core only: %q", got)
	}
	if got := names(map[string]string{"VPN_TYPE": "wireguard", "SERVER_COUNTRIES": "Sweden"}); got != "SERVER_HOSTNAMES=se-mma-wg-001 " {
		t.Errorf("a service without server names goes by hostname: %q", got)
	}
	if got := names(map[string]string{"SERVER_CITIES": "New York"}); got != "SERVER_NAMES=NY#1 " {
		t.Errorf("no type set is OpenVPN, as in gluetun: %q", got)
	}
}

// Every server that fits is a candidate, with every address gluetun may use for it.
func TestCandidatesAllWithAddresses(t *testing.T) {
	var list []serverEntry
	for i := 0; i < 120; i++ {
		list = append(list, serverEntry{VPN: "wireguard", City: "New York", ServerName: "NY#" + string(rune('A'+i%26)) + string(rune('a'+i/26)), IPs: []string{"192.0.2.1"}})
	}
	list = append(list, serverEntry{VPN: "wireguard", City: "New York", ServerName: "NY#Aa", IPs: []string{"192.0.2.2"}})
	got := candidatesFrom(list, map[string]string{"VPN_TYPE": "wireguard", "SERVER_CITIES": "New York"})
	if len(got) != 120 {
		t.Fatalf("%d candidates, want all 120", len(got))
	}
	for _, c := range got {
		if c.name == "NY#Aa" && len(c.ips) != 2 {
			t.Errorf("NY#Aa: addresses %v, want both", c.ips)
		}
	}
}
