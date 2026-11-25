package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func testFront() *front {
	return newFront(provider{Name: "One", URL: "http://provider.example:8080/", User: "real user", Password: "p@ss&1", Port: "8095", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
}

func TestSwap(t *testing.T) {
	f := testFront()
	for _, c := range [][3]string{ // request, pass-through address, address for the relay
		{"/live/tv/easy/12.ts", "/live/real%20user/p%40ss%261/12.ts", ""},
		{"//live//tv/easy/12.m3u8", "/live/real%20user/p%40ss%261/12.m3u8", ""},
		{"/movie/tv/easy/a%20b.mkv", "/movie/real%20user/p%40ss%261/a%20b.mkv", ""},
		{"/series/tv/easy/7.mp4?x=1", "/series/real%20user/p%40ss%261/7.mp4?x=1", ""},
		{"/timeshift/tv/easy/60/2026-01-01:10-00/5.ts", "/timeshift/real%20user/p%40ss%261/60/2026-01-01:10-00/5.ts", ""},
		{"/tv/easy/12", "/real%20user/p%40ss%261/12", ""},
		{"/tv/easy/12.ts", "/real%20user/p%40ss%261/12.ts", ""},
		{"/live/other/login/12.ts", "/live/other/login/12.ts", ""},
		{"/live/tv/wrong/12.ts", "/live/tv/wrong/12.ts", ""},
		{"/player_api.php?username=tv&password=easy&action=get_live_streams", "/player_api.php?username=real+user&password=p%40ss%261&action=get_live_streams", ""},
		{"/player_api.php?password=easy&a=b&username=tv", "/player_api.php?password=p%40ss%261&a=b&username=real+user", ""},
		{"/player_api.php?username=someone&password=else", "/player_api.php?username=someone&password=else", ""},
		{"/xmltv.php?username=tv&password=nope", "/xmltv.php?username=real+user&password=nope", ""},
		{"/__ch/live/tv/easy/12.m3u8/__index.m3u8?t=1", "/__ch/live/tv/easy/12.m3u8/__index.m3u8?t=1", "/live/real%20user/p%40ss%261/12.m3u8"},
		{"/__ch/tv/easy/12.m3u8/seg_4.ts", "/__ch/tv/easy/12.m3u8/seg_4.ts", "/real%20user/p%40ss%261/12.m3u8"},
		{"/__ch/live/a/b/12.m3u8/__index.m3u8", "/__ch/live/a/b/12.m3u8/__index.m3u8", "/live/a/b/12.m3u8"},
		{"/hls/tv/easy/12_1.ts?t=1", "/hls/real%20user/p%40ss%261/12_1.ts?t=1", ""},
		{"/__cdn/http/cdn.example/tv/easy/1.ts", "/__cdn/http/cdn.example/tv/easy/1.ts", ""},
	} {
		u, err := url.ParseRequestURI(c[0])
		if err != nil {
			t.Fatal(err)
		}
		up, uri := f.swap(u)
		if c[2] == "" {
			c[2] = c[1]
		}
		if up != c[1] || uri != c[2] {
			t.Errorf("%s: got %s and %s, want %s and %s", c[0], up, uri, c[1], c[2])
		}
	}
}

func TestLiveMatch(t *testing.T) {
	f := testFront()
	for p, want := range map[string]bool{"/live/tv/easy/12.ts": true, "/tv/easy/12": true, "/tv/easy/12.ts": true, "//live//tv//easy//12.ts": true,
		"/tv/easy/abc": false, "/live/tv/easy/12.m3u8": false, "/movie/tv/easy/12.ts": false, "/live/x/y/12.ts": false, "/live/tv/easy/a/12.ts": false,
		"/live/tv/easy/12_4031.ts": false, "/live/tv/easy/seg-7.ts": false} {
		if f.liveTS.MatchString(p) != want {
			t.Errorf("%s: live match should be %v", p, want)
		}
	}
}

func TestRewriteLocation(t *testing.T) {
	for _, c := range [][2]string{
		{"/hls/a.m3u8?t=1", "/__cdn/https/host.example:8443/hls/a.m3u8?t=1"},
		{"//cdn.example/x/y.ts", "/__cdn/https/cdn.example/x/y.ts"},
		{"http://cdn.example:8080/live/1.ts?token=a%2Fb", "/__cdn/http/cdn.example:8080/live/1.ts?token=a%2Fb"},
		{"https://cdn.example/a", "/__cdn/https/cdn.example/a"},
		{"HTTPS://CDN.example/a", "/__cdn/https/CDN.example/a"},
		{"/", "/"},
		{"next.ts", "next.ts"},
		{"?page=2", "?page=2"},
		{"ftp://x/y", "ftp://x/y"},
	} {
		if got := rewriteLocation(c[0], "https", "host.example:8443"); got != c[1] {
			t.Errorf("%s: got %s, want %s", c[0], got, c[1])
		}
	}
}

func TestCDN(t *testing.T) {
	for _, c := range [][3]string{ // decoded path, path as written, real address ("" = not one)
		{"/__cdn/https/cdn.example/a/b.ts", "/__cdn/https/cdn.example/a/b.ts", "https://cdn.example/a/b.ts?q=1"},
		{"/__cdn/http/cdn.example:8080/a b/c==.ts", "/__cdn/http/cdn.example:8080/a%20b/c%3D%3D.ts", "http://cdn.example:8080/a%20b/c%3D%3D.ts?q=1"},
		{"/__cdn/http/cdn.example", "/__cdn/http/cdn.example", "http://cdn.example/?q=1"},
		{"/__cdn/http/cdn.example//a/../b", "/__cdn/http/cdn.example//a/../b", "http://cdn.example/b?q=1"},
		{"/__cdn/HTTP/cdn.example/a", "/__cdn/HTTP/cdn.example/a", ""},
		{"/__cdn/ftp/cdn.example/a", "/__cdn/ftp/cdn.example/a", ""},
		{"/__cdn/", "/__cdn/", ""},
	} {
		_, _, target, ok := cdnTarget(c[0], c[1], "q=1")
		if target != c[2] || ok != (c[2] != "") {
			t.Errorf("%s: got %q (%v), want %q", c[1], target, ok, c[2])
		}
	}
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST:80": true, "127.0.0.1": true, "10.1.2.3": true, "192.168.1.1:8080": true,
		"172.16.0.1": true, "172.31.9.9": true, "172.32.0.1": false, "169.254.1.1": true, "100.64.0.1": true, "100.127.0.1": true, "100.128.0.1": false,
		"0.0.0.0": true, "0177.0.0.1": true, "010.1.1.1": true, "[::1]": true, "user@cdn.example": true, "cdn%2eexample": true,
		"cdn.example": false, "203.0.113.5:9000": false, "8.8.8.8": false} {
		if got := cdnForbidden.MatchString(cleanPath("/__cdn/http/" + host + "/x")); got != want {
			t.Errorf("%s: forbidden should be %v", host, want)
		}
	}
	for _, loose := range []string{"2130706433", "0x7f000001", "127.1", "localhost"} { // refused by the name check
		if home, _ := leadsHome(nil, loose); !home {
			t.Errorf("%s should lead home", loose)
		}
	}
}

func goodSettings() *settings {
	s := &settings{}
	s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{"name":"One","url":"https://provider.example","user":"u","password":"p","port":8095,"connections":2,"live_tv":"ts"},
		{"name":"Two","url":"http://two.example:8080/","user":"u2","password":"p2","port":"8096","connections":"1","live_tv":"hls"}],"ui":{"port":8195},
		"vpn":{"service_provider":"mullvad","type":"wireguard","wireguard_private_key":"KEY","wireguard_addresses":"10.64.0.2/32","server_countries":"","update_hours":48,
		"extra":{"TZ":"America/New_York","OPENVPN_PASSWORD":"secret","Some_New-Name":"1","SERVER_CITIES":"Chicago IL"},"server_cities":"New York NY"}}`))
	return s
}

func TestValidate(t *testing.T) {
	if errs := goodSettings().validate(); len(errs) != 0 {
		t.Fatalf("good settings refused: %v", errs)
	}
	for field, change := range map[string]func(*settings){
		"player.user":            func(s *settings) { s.Player.User = "live" },
		"player.password":        func(s *settings) { s.Player.Password = "a b" },
		"ui.port":                func(s *settings) { s.UI.Port = "8000" },
		"providers[0].name":      func(s *settings) { s.Providers[0].Name = "" },
		"providers[1].name":      func(s *settings) { s.Providers[1].Name = "one" },
		"providers[0].url":       func(s *settings) { s.Providers[0].URL = "https://provider.example/path" },
		"providers[0].port":      func(s *settings) { s.Providers[0].Port = "70000" },
		"providers[1].port":      func(s *settings) { s.Providers[1].Port = "8095" },
		"providers[1].user":      func(s *settings) { s.Providers[1].User = "a/b" },
		"providers[0].password":  func(s *settings) { s.Providers[0].Password = "" },
		"providers[0].live_tv":   func(s *settings) { s.Providers[0].LiveTV = "dash" },
		"vpn.service_provider":   func(s *settings) { s.VPN.Vars["service_provider"] = "" },
		"vpn.extra.A=B":          func(s *settings) { s.VPN.Extra["A=B"] = "1" },
		"vpn.update_hours":       func(s *settings) { s.VPN.Vars["update_hours"] = "often" },
		"providers[0].port#ui":   func(s *settings) { s.Providers[0].Port = "8195" },
		"providers[0].port#9999": func(s *settings) { s.Providers[0].Port = "9999" },
	} {
		s := goodSettings()
		change(s)
		errs := s.validate()
		want, _, _ := strings.Cut(field, "#")
		if len(errs) != 1 || errs[0].Field != want {
			t.Errorf("%s: got %v", field, errs)
		}
	}
}

func TestVPNEnv(t *testing.T) {
	env := goodSettings().vpnEnv()
	for k, want := range map[string]string{"VPN_SERVICE_PROVIDER": "mullvad", "VPN_TYPE": "wireguard", "WIREGUARD_PRIVATE_KEY": "KEY",
		"WIREGUARD_ADDRESSES": "10.64.0.2/32", "UPDATER_PERIOD": "48h", "TZ": "America/New_York", "OPENVPN_PASSWORD": "secret",
		"Some_New-Name": "1", "SERVER_CITIES": "Chicago IL"} { // (extra wins over the friendly field)
		if env[k] != want {
			t.Errorf("%s: got %q, want %q", k, env[k], want)
		}
	}
	if _, set := env["SERVER_COUNTRIES"]; set || len(env) != 9 {
		t.Errorf("an empty setting must not be passed on: %d variables", len(env))
	}
	s := goodSettings()
	delete(s.VPN.Vars, "update_hours")
	if got := s.vpnEnv()["UPDATER_PERIOD"]; got != "24h" {
		t.Errorf("the server list should be updated once a day unless set otherwise: got %q", got)
	}
	s.VPN.Vars["update_hours"] = "0"
	if got := s.vpnEnv()["UPDATER_PERIOD"]; got != "0" {
		t.Errorf("update_hours 0 turns the update off: got %q", got)
	}
	s.VPN.Extra["UPDATER_PERIOD"] = "360h"
	if got := s.vpnEnv()["UPDATER_PERIOD"]; got != "360h" {
		t.Errorf("extra wins: got %q", got)
	}
}

func TestMaskAndKeep(t *testing.T) {
	old := goodSettings()
	shown, _ := json.Marshal(old.masked())
	for _, secret := range []string{"easy", `"p"`, `"p2"`, "KEY", "secret"} {
		if strings.Contains(string(shown), secret) {
			t.Errorf("the masked settings show %s", secret)
		}
	}
	for _, note := range []string{`"password_set":true`, `"wireguard_private_key_set":true`, `"extra_set":{"OPENVPN_PASSWORD":true}`, `"TZ":"America/New_York"`} {
		if !strings.Contains(string(shown), note) {
			t.Errorf("the masked settings lack %s", note)
		}
	}
	// what the page sends back: secrets empty, the first provider removed, the second renamed, one new
	s := &settings{}
	if err := s.parse(shown); err != nil {
		t.Fatal(err)
	}
	s.Providers = append(s.Providers[1:], provider{Name: "New", URL: "https://new.example", User: "n", Port: "8097", LiveTV: "ts"})
	s.Providers[0].Name = "Renamed"
	delete(s.VPN.Vars, "update_hours") // (a setting the page does not send)
	s.VPN.Vars["server_cities"] = ""   // (a setting the page cleared)
	s.keepSecrets(old)
	if s.VPN.Vars["update_hours"] != "48" || s.VPN.Vars["server_cities"] != "" {
		t.Errorf("a VPN setting left out must be kept, one sent empty cleared: %v", s.VPN.Vars)
	}
	if s.Player.Password != "easy" || s.Providers[0].Password != "p2" || s.Providers[1].Password != "" ||
		s.VPN.Vars["wireguard_private_key"] != "KEY" || s.VPN.Extra["OPENVPN_PASSWORD"] != "secret" {
		t.Errorf("secrets not kept as they should be: %+v", s)
	}
	if errs := s.validate(); len(errs) != 1 || errs[0].Field != "providers[1].password" {
		t.Errorf("a new provider without a password: got %v", errs)
	}
	if strings.Contains(string(old.forProxy()), "KEY") || strings.Contains(string(old.forProxy()), "mullvad") {
		t.Errorf("the proxy part is handed VPN settings")
	}
}
